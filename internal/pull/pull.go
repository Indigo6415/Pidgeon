// Package pull is the `pidgeon pull [url] [dir]` subcommand: it downloads a
// folder (or one file) from another Pidgeon server straight into a local
// directory, keeping its structure, and `pidgeon find`, which lists the
// servers on the network.
//
// How it works:
//
//  1. The tree is discovered with /api/ls (the web UI's API; --token goes
//     along as a Bearer header), or through a share link's ?json=1 pages.
//  2. Folders are recreated under the target directory. All writes go
//     through an os.Root, so no name from the server can escape it.
//  3. Each file is fetched from /f/<path> into "<name>.part" and renamed
//     when complete; a leftover .part is resumed with a Range request, and
//     dropped connections are retried the same way. The file gets the
//     server's modification time.
//  4. A few files download in parallel, with one live line each.
//
// Files already there with the same size and modification time are
// skipped, so running it again only fetches what's new; --mirror also
// replaces files that changed on the server, and --verify checks every
// file against the server's SHA-256.
package pull

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/Indigo6415/pidgeon/internal/brand"
	"github.com/Indigo6415/pidgeon/internal/certs"
	"github.com/Indigo6415/pidgeon/internal/humanize"
	"github.com/Indigo6415/pidgeon/internal/mdns"
)

// attempts is how many times a download is tried (resuming each time).
const attempts = 4

// options are the parsed flags.
type options struct {
	token, pin, fingerprint string
	mirror, verify          bool
	jobs                    int
}

// Main runs `pidgeon pull` and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	var o options
	fl := flag.NewFlagSet(brand.Command+" pull", flag.ContinueOnError)
	fl.SetOutput(stderr)
	fl.StringVar(&o.token, "token", "", "access token for a protected server (or put ?token= in the URL)")
	fl.StringVar(&o.pin, "pin", "", "PIN of a share link that asks for one")
	fl.StringVar(&o.fingerprint, "fingerprint", "", "trust an HTTPS server whose certificate has this SHA-256 fingerprint")
	fl.BoolVar(&o.mirror, "mirror", false, "also replace local files that differ from the server's")
	fl.BoolVar(&o.verify, "verify", false, "check every file against the server's SHA-256")
	fl.IntVar(&o.jobs, "j", 4, "files to download at once")
	fl.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s pull [flags] [url] [target-dir]\n\n", brand.Command)
		fmt.Fprintln(stderr, "Download a folder (or a file) from another "+brand.Name+" server into target-dir,")
		fmt.Fprintln(stderr, "keeping its structure. Without a URL, servers on this network are found")
		fmt.Fprintln(stderr, "automatically. The URL may be the address, a link copied from the web UI")
		fmt.Fprintln(stderr, "(a folder, http://host:8080/#/Photos), or a share link (/s/...).")
		fmt.Fprintln(stderr, "\nFiles already there are skipped, so running it again fetches only what's new.")
		fmt.Fprintln(stderr, "\nFlags:")
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	rest := fl.Args()
	if len(rest) > 2 || o.jobs < 1 {
		fl.Usage()
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// The first argument is a URL if it has a scheme; otherwise find a server.
	var raw, target string
	switch {
	case len(rest) > 0 && (strings.HasPrefix(rest[0], "http://") || strings.HasPrefix(rest[0], "https://")):
		raw = rest[0]
		if len(rest) == 2 {
			target = rest[1]
		}
	case len(rest) == 2:
		fmt.Fprintf(stderr, "%s pull: %q is not an http(s) URL\n", brand.Command, rest[0])
		return 2
	default:
		if len(rest) == 1 {
			target = rest[0]
		}
		peer, code := choosePeer(ctx, stderr)
		if code >= 0 {
			return code
		}
		raw = peer.URL() + "/"
		if fp := certs.Normalize(peer.TXT["fp"]); fp != "" && o.fingerprint == "" {
			o.fingerprint = fp // announced by the server on this network
		}
	}
	if err := run(ctx, raw, target, o, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "%s pull: %v\n", brand.Command, err)
		return 1
	}
	return 0
}

// remote is what a URL points at.
type remote struct {
	base  string // "http(s)://host:port"
	path  string // remote path, "/" or "/a/b"
	share string
	token string
	user  string
	pass  string
}

// parseURL understands the server address, folder links from the web UI
// ("#/a/b"), file links ("/f/a/b") and share links ("/s/<id>[/sub]").
func parseURL(raw string) (remote, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return remote{}, fmt.Errorf("%q is not an http(s) URL", raw)
	}
	r := remote{base: u.Scheme + "://" + u.Host, path: "/", token: u.Query().Get("token")}
	if u.User != nil {
		r.user = u.User.Username()
		r.pass, _ = u.User.Password()
	}
	p := u.Path
	switch {
	case strings.HasPrefix(p, "/s/"):
		id, sub, _ := strings.Cut(strings.TrimPrefix(p, "/s/"), "/")
		r.share = id
		r.path = "/" + strings.Trim(sub, "/")
	case strings.HasPrefix(p, "/f/"):
		r.path = "/" + strings.Trim(strings.TrimPrefix(p, "/f/"), "/")
	case u.Fragment != "":
		r.path = "/" + strings.Trim(u.Fragment, "/")
	}
	r.path = path.Clean(r.path)
	return r, nil
}

// run pulls raw into target.
func run(ctx context.Context, raw, target string, o options, stdout, stderr io.Writer) error {
	rm, err := parseURL(raw)
	if err != nil {
		return err
	}
	tlsCfg, err := trust(ctx, rm.base, o.fingerprint, stderr)
	if err != nil {
		return err
	}
	c := newClient(rm.base, tlsCfg)
	c.share, c.pin, c.user, c.pass = rm.share, o.pin, rm.user, rm.pass
	c.token = o.token
	if c.token == "" {
		c.token = rm.token
	}

	top, err := c.stat(ctx, rm.path)
	if err != nil {
		return err
	}
	// The target defaults to the remote folder's name, like `git clone`.
	if target == "" {
		switch {
		case !top.Dir:
			target = "."
		case rm.path != "/":
			target = path.Base(rm.path)
		default:
			target = safeName(c.rootName(ctx))
			if target == "" {
				target = "pidgeon-pull"
			}
		}
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer root.Close()

	p := newProgress(stderr)
	pctx, stopProgress := context.WithCancel(ctx)
	var pwg sync.WaitGroup
	pwg.Add(1)
	go func() { defer pwg.Done(); p.run(pctx) }()

	pl := &puller{c: c, root: root, o: o, p: p, jobs: make(chan job, 256)}
	var wg sync.WaitGroup
	// Workers download while the lister is still walking the tree.
	for range o.jobs {
		wg.Add(1)
		go func() { defer wg.Done(); pl.work(ctx) }()
	}
	var walkErr error
	if top.Dir {
		walkErr = pl.walk(ctx, rm.path, ".")
	} else {
		name := safeName(top.Name)
		if name == "" {
			name = safeName(path.Base(rm.path))
		}
		pl.queue(job{remote: rm.path, local: name, size: top.Size, mtime: top.MTime})
	}
	p.listing.Store(false)
	close(pl.jobs)
	wg.Wait()
	stopProgress()
	pwg.Wait()

	// The summary goes to stdout so scripts can capture it.
	fmt.Fprintf(stdout, "%s: %d downloaded (%s), %d already there", target, pl.got, humanize.Bytes(p.bytesDone.Load()), pl.skipped)
	if pl.verified > 0 {
		fmt.Fprintf(stdout, ", %d verified", pl.verified)
	}
	if pl.conflicts > 0 {
		fmt.Fprintf(stdout, ", %d differ (use --mirror to replace them)", pl.conflicts)
	}
	fmt.Fprintln(stdout)
	switch {
	case ctx.Err() != nil:
		return errors.New("interrupted; run the same command again to resume")
	case walkErr != nil:
		return walkErr
	case pl.failed > 0:
		return fmt.Errorf("%d file(s) failed", pl.failed)
	}
	return nil
}

// job is one file to fetch.
type job struct {
	remote string // remote path
	local  string // path under the target directory
	size   int64
	mtime  int64 // Unix ms
}

// puller holds the state of one pull.
type puller struct {
	c    *client
	root *os.Root
	o    options
	p    *progress
	jobs chan job

	mu                                        sync.Mutex
	got, skipped, conflicts, failed, verified int
}

// count bumps one of the result counters.
func (pl *puller) count(n *int) {
	pl.mu.Lock()
	*n++
	pl.mu.Unlock()
}

// queue hands a file to the workers.
func (pl *puller) queue(j job) {
	pl.p.filesTotal.Add(1)
	pl.p.bytesTotal.Add(max(0, j.size))
	pl.jobs <- j
}

// walk lists the remote folder p (local folder dir) and everything below.
func (pl *puller) walk(ctx context.Context, p, dir string) error {
	l, err := pl.c.list(ctx, p)
	if err != nil {
		return fmt.Errorf("listing %s: %w", p, err)
	}
	if dir != "." {
		if err := pl.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	var subdirs []entry
	// Files first, so downloads start before the walk goes deeper.
	for _, e := range l.Entries {
		name := safeName(e.Name)
		if name == "" {
			pl.p.log("  ! skipped %q: not a safe file name here", e.Name)
			continue
		}
		if e.Dir {
			subdirs = append(subdirs, e)
			continue
		}
		pl.queue(job{remote: path.Join(p, e.Name), local: path.Join(dir, name), size: e.Size, mtime: e.MTime})
	}
	for _, e := range subdirs {
		if ctx.Err() != nil {
			return nil
		}
		if err := pl.walk(ctx, path.Join(p, e.Name), path.Join(dir, safeName(e.Name))); err != nil {
			pl.p.log("  ! %v", err)
			pl.count(&pl.failed)
		}
	}
	return nil
}

// safeName accepts a name from the server only if it is one plain path
// segment (os.Root would refuse an escape anyway; this keeps errors clear).
func safeName(n string) string {
	if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\\\x00") {
		return ""
	}
	return n
}

// work processes jobs until the queue closes.
func (pl *puller) work(ctx context.Context) {
	for j := range pl.jobs {
		if ctx.Err() != nil {
			continue // drain the queue
		}
		pl.one(ctx, j)
	}
}

// sameTime reports whether two modification times match, allowing for
// filesystems that keep only whole (or even) seconds.
func sameTime(a time.Time, ms int64) bool {
	d := a.Sub(time.UnixMilli(ms))
	return d > -2*time.Second && d < 2*time.Second
}

// one brings one file up to date.
func (pl *puller) one(ctx context.Context, j job) {
	if fi, err := pl.root.Stat(j.local); err == nil {
		same := fi.Mode().IsRegular() && fi.Size() == j.size && sameTime(fi.ModTime(), j.mtime)
		if same && pl.o.verify {
			// Identical on the outside; check the inside too.
			if ok, err := pl.check(ctx, j); err != nil {
				pl.p.log("  ! %s: can't verify: %v", j.local, err)
			} else if !ok {
				same = false
				pl.p.log("  ! %s: content differs from the server", j.local)
			} else {
				pl.count(&pl.verified)
			}
		}
		if same {
			pl.count(&pl.skipped)
			pl.p.filesDone.Add(1)
			pl.p.bytesTotal.Add(-max(0, j.size))
			return
		}
		if !pl.o.mirror {
			pl.count(&pl.conflicts)
			pl.p.log("  ≠ %s differs from the server's copy (kept; --mirror replaces it)", j.local)
			pl.p.filesDone.Add(1)
			pl.p.bytesTotal.Add(-max(0, j.size))
			return
		}
	}
	t := pl.p.begin(j.local, j.size)
	err := pl.fetch(ctx, j, t)
	pl.p.end(t)
	pl.p.filesDone.Add(1)
	if err != nil {
		if ctx.Err() == nil {
			pl.count(&pl.failed)
			pl.p.log("  ✗ %s: %v", j.local, err)
		}
		return
	}
	if pl.o.verify {
		ok, err := pl.check(ctx, j)
		switch {
		case err != nil:
			pl.p.log("  ! %s: can't verify: %v", j.local, err)
		case !ok:
			// Remove it so the next run fetches it again.
			pl.root.Remove(j.local)
			pl.count(&pl.failed)
			pl.p.log("  ✗ %s: checksum mismatch, removed", j.local)
			return
		default:
			pl.count(&pl.verified)
		}
	}
	pl.count(&pl.got)
	pl.p.log("  ✓ %s  %s", j.local, humanize.Bytes(j.size))
}

// fetch downloads a file into place, resuming a leftover ".part" and
// retrying dropped connections.
func (pl *puller) fetch(ctx context.Context, j job, t *task) error {
	part := j.local + ".part"
	var lastErr error
	for try := range attempts {
		if try > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(try) * time.Second):
			}
		}
		done, err := pl.fetchOnce(ctx, j, part, t)
		if err == nil && (j.size < 0 || done == j.size) {
			pl.root.Chtimes(part, time.Now(), time.UnixMilli(j.mtime))
			return pl.root.Rename(part, j.local)
		}
		if err == nil {
			err = fmt.Errorf("got %s of %s", humanize.Bytes(done), humanize.Bytes(j.size))
		}
		lastErr = err
		// Errors the server meant (404, 403, ...) won't go away by retrying.
		var se statusError
		if errors.As(err, &se) || ctx.Err() != nil {
			break
		}
	}
	return lastErr
}

// statusError is an HTTP error answer from the server.
type statusError struct{ status string }

// Error implements error.
func (e statusError) Error() string { return "server said " + e.status }

// fetchOnce makes one request for the file, continuing the ".part" file if
// there is one. It returns the size of the .part file afterwards.
func (pl *puller) fetchOnce(ctx context.Context, j job, part string, t *task) (int64, error) {
	have := int64(0)
	if fi, err := pl.root.Stat(part); err == nil && (j.size < 0 || fi.Size() <= j.size) {
		have = fi.Size()
	}
	hdr := http.Header{}
	if have > 0 {
		hdr.Set("Range", fmt.Sprintf("bytes=%d-", have))
		// Only continue if it is still the same version of the file.
		hdr.Set("If-Range", time.UnixMilli(j.mtime).UTC().Format(http.TimeFormat))
	}
	resp, err := pl.c.get(ctx, pl.c.fileURL(j.remote), hdr)
	if err != nil {
		return have, err
	}
	defer resp.Body.Close()
	flags := os.O_WRONLY | os.O_CREATE
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusOK:
		flags |= os.O_TRUNC // a fresh start: the server sends everything
		have = 0
	case http.StatusRequestedRangeNotSatisfiable:
		// The .part is already complete (or bogus): start over next time.
		pl.root.Remove(part)
		return 0, errors.New("server can't resume this file; retrying from the start")
	default:
		return have, statusError{resp.Status}
	}
	f, err := pl.root.OpenFile(part, flags, 0o644)
	if err != nil {
		return have, err
	}
	// Bytes already on disk count as progress too.
	t.done.Store(have)
	n, err := io.CopyBuffer(f, &countReader{r: resp.Body, pl: pl, t: t}, make([]byte, 256<<10))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return have + n, err
}

// countReader reports bytes as they arrive.
type countReader struct {
	r  io.Reader
	pl *puller
	t  *task
}

// Read reads and counts.
func (c *countReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.pl.p.add(c.t, n)
	return n, err
}

// check compares a local file's SHA-256 with the server's.
func (pl *puller) check(ctx context.Context, j job) (bool, error) {
	want, err := pl.c.hash(ctx, j.remote)
	if err != nil {
		return false, err
	}
	if want == "" {
		return true, nil // the server can't say (share links): trust the size
	}
	f, err := pl.root.Open(j.local)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}

// trust decides how to check the server's certificate: pinned to a known
// fingerprint, or the system's checks. A self-signed server with no known
// fingerprint is shown to the user, who can accept it on a terminal.
func trust(ctx context.Context, base, fingerprint string, stderr io.Writer) (*tls.Config, error) {
	if !strings.HasPrefix(base, "https://") {
		return nil, nil
	}
	if fingerprint != "" {
		fp := certs.Normalize(fingerprint)
		if fp == "" {
			return nil, errors.New("--fingerprint must be a SHA-256 fingerprint (64 hex digits)")
		}
		return certs.Pin(fp), nil
	}
	host := strings.TrimPrefix(base, "https://")
	d := tls.Dialer{Config: &tls.Config{}}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err == nil {
		conn.Close()
		return nil, nil // a certificate the system already trusts
	}
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostErr x509.HostnameError
	if !errors.As(err, &unknown) && !errors.As(err, &invalid) && !errors.As(err, &hostErr) {
		return nil, err
	}
	// Self-signed: look at it without trusting it, and ask.
	fp, err := peek(ctx, host)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(stderr, "The server uses a self-signed certificate with SHA-256 fingerprint\n  %s\n", certs.Pretty(fp, 0))
	fmt.Fprintln(stderr, "Compare it with the one in the host's dashboard.")
	if !isTerminal(os.Stdin) {
		return nil, fmt.Errorf("not trusted; to trust it, pass --fingerprint %s", fp)
	}
	fmt.Fprint(stderr, "Trust it for this download? [y/N] ")
	ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
		return nil, errors.New("certificate not trusted")
	}
	return certs.Pin(fp), nil
}

// peek returns the fingerprint of the certificate a server presents.
func peek(ctx context.Context, host string) (string, error) {
	d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} // only to read the certificate
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	cs := conn.(*tls.Conn).ConnectionState()
	if len(cs.PeerCertificates) == 0 {
		return "", errors.New("the server sent no certificate")
	}
	sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:]), nil
}

// isTerminal reports whether f is an interactive terminal.
func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// browseTime is how long the network is listened to for servers.
const browseTime = 1500 * time.Millisecond

// choosePeer finds the servers on the network and picks one: the only one,
// or the user's choice on a terminal. code >= 0 means exit with it.
func choosePeer(ctx context.Context, stderr io.Writer) (mdns.Peer, int) {
	fmt.Fprintln(stderr, "looking for "+brand.Name+" servers on this network…")
	bctx, cancel := context.WithTimeout(ctx, browseTime)
	peers, err := mdns.Browse(bctx)
	cancel()
	switch {
	case len(peers) == 0 && err != nil:
		fmt.Fprintf(stderr, "%s pull: can't search the network: %v\n", brand.Command, err)
		return mdns.Peer{}, 1
	case len(peers) == 0:
		fmt.Fprintf(stderr, "%s pull: no servers found; pass the URL shown by the server instead\n", brand.Command)
		return mdns.Peer{}, 1
	case len(peers) == 1:
		fmt.Fprintf(stderr, "found %s at %s\n", peers[0].Instance, peers[0].URL())
		return peers[0], -1
	}
	printPeers(stderr, peers, true)
	if !isTerminal(os.Stdin) {
		fmt.Fprintf(stderr, "%s pull: several servers found; pass the URL of the one you want\n", brand.Command)
		return mdns.Peer{}, 2
	}
	fmt.Fprint(stderr, "Pull from which one? [1] ")
	ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	n := 1
	fmt.Sscan(strings.TrimSpace(ans), &n)
	if n < 1 || n > len(peers) {
		fmt.Fprintf(stderr, "%s pull: no server number %d\n", brand.Command, n)
		return mdns.Peer{}, 2
	}
	return peers[n-1], -1
}

// printPeers lists servers, numbered when the user is to choose.
func printPeers(w io.Writer, peers []mdns.Peer, numbered bool) {
	nameW := 0
	for _, p := range peers {
		nameW = max(nameW, len([]rune(p.Instance)))
	}
	for i, p := range peers {
		lead := "  "
		if numbered {
			lead = fmt.Sprintf("  %d) ", i+1)
		}
		notes := []string{authLabel(p.TXT["auth"])}
		if fp := certs.Normalize(p.TXT["fp"]); fp != "" {
			notes = append(notes, "SHA-256 "+certs.Pretty(fp, 4))
		}
		pad := strings.Repeat(" ", nameW-len([]rune(p.Instance)))
		fmt.Fprintf(w, "%s%s%s  %s  %s\n", lead, p.Instance, pad, p.URL(), strings.Join(notes, " · "))
	}
}

// authLabel describes a server's access gate.
func authLabel(a string) string {
	switch a {
	case "token":
		return "needs a token"
	case "basic":
		return "needs a password"
	case "token + basic":
		return "needs a token or password"
	}
	return "open"
}

// Find runs `pidgeon find`: list the servers on the network.
func Find(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet(brand.Command+" find", flag.ContinueOnError)
	fl.SetOutput(stderr)
	wait := fl.Duration("wait", 2*time.Second, "how long to listen for answers")
	fl.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s find [--wait 2s]\n\nList the %s servers on this network.\n\n", brand.Command, brand.Name)
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *wait)
	defer cancel()
	peers, err := mdns.Browse(ctx)
	if len(peers) == 0 {
		if err != nil {
			fmt.Fprintf(stderr, "%s find: can't search the network: %v\n", brand.Command, err)
			return 1
		}
		fmt.Fprintln(stdout, "no "+brand.Name+" servers found on this network")
		return 0
	}
	printPeers(stdout, peers, false)
	return 0
}
