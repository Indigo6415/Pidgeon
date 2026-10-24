// Package app wires flags, listeners, the HTTP server, stats and the TUI
// together. Both binaries (cmd/pidgeon and cmd/pdgn) call Main.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"charm.land/lipgloss/v2"
	"golang.org/x/term"

	"github.com/Indigo6415/pidgeon/internal/brand"
	"github.com/Indigo6415/pidgeon/internal/certs"
	"github.com/Indigo6415/pidgeon/internal/devices"
	"github.com/Indigo6415/pidgeon/internal/humanize"
	"github.com/Indigo6415/pidgeon/internal/listeners"
	"github.com/Indigo6415/pidgeon/internal/mdns"
	"github.com/Indigo6415/pidgeon/internal/pull"
	"github.com/Indigo6415/pidgeon/internal/server"
	"github.com/Indigo6415/pidgeon/internal/stats"
	"github.com/Indigo6415/pidgeon/internal/tui"
	"github.com/Indigo6415/pidgeon/internal/web"
)

// shutdownGrace is how long in-flight transfers get to finish on quit
// before their connections are closed.
const shutdownGrace = 3 * time.Second

// config is the parsed command line.
type config struct {
	paths    []string // directories and files to serve; one directory is the usual case
	port     int
	bind     string
	token    string
	auth     string
	hidden   bool
	noSplash bool
	headless bool
	version  bool

	ask      bool          // approve each new device (and upload) in the dashboard
	inbox    string        // upload directory; "" disables uploads
	inboxMax string        // largest accepted upload, e.g. "2G"
	expire   time.Duration // stop after this long; 0 = never
	once     bool          // stop after the first completed download
	maxBytes string        // stop once this much has been served, e.g. "5G"

	limit       string // cap on all downloads together, e.g. "5M" (per second)
	clientLimit string // cap on each device's downloads
	noThumbs    bool   // no image thumbnails in the web UI's grid view

	inboxSort string // "date": file uploads into YYYY/MM folders
	tls       string // "auto" or "cert.pem,key.pem": serve HTTPS
	tv        bool   // act as a DLNA media server for TVs
	tvName    string // the name TVs show
	noMDNS    bool   // don't announce the server on the network
	flock     bool   // let visitors browse the Nearby servers through this one
	receipts  string // append delivery receipts to this JSON Lines file
}

// Reasons the server stops on its own (--expire, --once, --max-bytes); the
// cause is reported in the shutdown summary.
var (
	errExpired = errors.New("time limit reached")
	errOnce    = errors.New("the download is complete (--once)")
	errBudget  = errors.New("data limit reached")
)

// Main runs the program with the given arguments (excluding argv[0]) and
// returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	// Subcommands come first; everything else is the server.
	if len(args) > 0 && args[0] == "pull" {
		return pull.Main(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "find" {
		return pull.Find(args[1:], stdout, stderr)
	}
	cfg, code := parseFlags(args, stderr)
	if code >= 0 {
		return code
	}
	if cfg.version {
		fmt.Fprintf(stdout, "%s %s\n", brand.Command, brand.ResolvedVersion())
		return 0
	}
	if err := serve(cfg, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", brand.Command, err)
		return 1
	}
	return 0
}

// parseFlags parses args. It returns code >= 0 when the program should exit
// immediately with that code (help or a usage error).
func parseFlags(args []string, stderr io.Writer) (config, int) {
	var c config
	fs := flag.NewFlagSet(brand.Command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.IntVar(&c.port, "port", 8080, "port to listen on (auto-increments if taken)")
	fs.IntVar(&c.port, "p", 8080, "shorthand for --port")
	fs.StringVar(&c.bind, "bind", "default", "listeners to enable: default, loopback, lan, vpn, virtual, all (wildcard), or IPs; comma-separated")
	fs.StringVar(&c.bind, "b", "default", "shorthand for --bind")
	fs.StringVar(&c.token, "token", "", `require an access token; "auto" generates a random one`)
	fs.StringVar(&c.auth, "auth", "", "require HTTP basic auth, as user:password")
	fs.BoolVar(&c.hidden, "hidden", false, "serve dotfiles and dot-directories (hidden by default)")
	fs.BoolVar(&c.noSplash, "no-splash", false, "skip the startup animation")
	fs.BoolVar(&c.headless, "headless", false, "no dashboard: print URLs and a request log (automatic when not a TTY)")
	fs.BoolVar(&c.version, "version", false, "print the version and exit")
	fs.BoolVar(&c.ask, "ask", false, "approve each new device (and each upload) in the dashboard before it gets anything")
	fs.StringVar(&c.inbox, "inbox", "", "let visitors upload files into this directory (created if missing)")
	fs.StringVar(&c.inboxMax, "inbox-max", "", "largest upload accepted, e.g. 2G (default: no limit)")
	fs.DurationVar(&c.expire, "expire", 0, "stop serving after this long, e.g. 30m or 2h")
	fs.BoolVar(&c.once, "once", false, "stop after the first completed download")
	fs.StringVar(&c.maxBytes, "max-bytes", "", "stop once this much has been served, e.g. 5G")
	fs.StringVar(&c.limit, "limit", "", "cap the total download speed, e.g. 5M (per second; changeable in the dashboard)")
	fs.StringVar(&c.clientLimit, "client-limit", "", "cap each device's download speed, e.g. 2M (per second)")
	fs.BoolVar(&c.noThumbs, "no-thumbs", false, "don't generate image thumbnails for the web UI's grid view")
	fs.StringVar(&c.inboxSort, "inbox-sort", "", `"date": file uploads into YYYY/MM folders by when they were taken (camera roll backups)`)
	fs.StringVar(&c.tls, "tls", "", `serve HTTPS: "auto" (a self-signed certificate, kept between runs) or "cert.pem,key.pem"`)
	fs.BoolVar(&c.tv, "tv", false, "also be a DLNA media server, so TVs and media players can browse and play videos, music and photos")
	fs.StringVar(&c.tvName, "tv-name", "", "the name TVs show for --tv (default: "+brand.Name+" · <folder>)")
	fs.BoolVar(&c.noMDNS, "no-mdns", false, "don't announce the server on the network (used by "+brand.Command+" find, pull and the web UI's Nearby list)")
	fs.BoolVar(&c.flock, "flock", false, "let visitors open the other "+brand.Name+" servers on the network in place, as folders, and mix their files into ZIPs")
	fs.StringVar(&c.receipts, "receipts", "", "append a delivery receipt (who got which file, when, its SHA-256) to this file, one JSON line each")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "%s — %s\n\n", brand.Name, brand.Tagline)
		fmt.Fprintf(stderr, "Usage:\n  %s [flags] [directory]\n  %s [flags] <path> <path>...   (several folders and files side by side)\n  %s pull [url] [target-dir]   (download a folder from another server)\n  %s find                      (list the servers on this network)\n\n", brand.Command, brand.Command, brand.Command, brand.Command)
		fmt.Fprintf(stderr, "%s is the same program as %s.\n\nFlags:\n", brand.Alias, brand.Command)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		// -h/--help is a successful exit; anything else is a usage error.
		if errors.Is(err, flag.ErrHelp) {
			return c, 0
		}
		return c, 2
	}
	c.paths = fs.Args()
	// No path: serve the current directory.
	if len(c.paths) == 0 {
		c.paths = []string{"."}
	}
	return c, -1
}

// serve runs the server until the user quits or a signal arrives.
func serve(cfg config, stdout, stderr io.Writer) error {
	paths := make([]string, len(cfg.paths))
	// Every path must exist; one path on its own must be a directory, and
	// several become top-level entries of a virtual root.
	for i, p := range cfg.paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		fi, err := os.Stat(abs)
		switch {
		case err != nil:
			return fmt.Errorf("%s: no such file or directory", p)
		case len(cfg.paths) == 1 && !fi.IsDir():
			// A single file is served on its own, like several paths.
		case !fi.IsDir() && !fi.Mode().IsRegular():
			return fmt.Errorf("%s is not a file or directory", p)
		}
		paths[i] = abs
	}
	dir := describePaths(paths)

	// Access control.
	token := cfg.token
	if token == "auto" {
		token = randomToken()
	}
	var user, pass string
	if cfg.auth != "" {
		var ok bool
		user, pass, ok = strings.Cut(cfg.auth, ":")
		if !ok || user == "" || pass == "" {
			return errors.New(`--auth must look like "user:password"`)
		}
	}

	// Session limits and the inbox.
	lim, err := parseLimits(cfg)
	if err != nil {
		return err
	}
	rates, err := parseRates(cfg)
	if err != nil {
		return err
	}
	switch {
	case cfg.inboxSort != "" && cfg.inboxSort != "date":
		return errors.New(`--inbox-sort: the only order is "date"`)
	case cfg.inboxSort != "" && cfg.inbox == "":
		return errors.New("--inbox-sort needs --inbox")
	case cfg.flock && cfg.noMDNS:
		// The flock is made of the servers the mDNS scan finds.
		return errors.New("--flock finds the other servers over mDNS, so it can't be combined with --no-mdns")
	case cfg.tv && cfg.tls != "":
		// TVs only speak plain HTTP and would reject a self-signed certificate.
		return errors.New("--tv can't be combined with --tls: TVs only play over plain HTTP")
	}
	var inboxMax int64
	if cfg.inboxMax != "" {
		if cfg.inbox == "" {
			return errors.New("--inbox-max needs --inbox")
		}
		if inboxMax, err = humanize.ParseBytes(cfg.inboxMax); err != nil {
			return fmt.Errorf("--inbox-max: %w", err)
		}
	}

	// The dashboard needs a terminal on both ends that this process owns;
	// anything else (pipes, services, containers, `&`) runs headless.
	foreground := isTTY(os.Stdin) && inForeground(os.Stdin)
	interactive := !cfg.headless && isTTY(os.Stdout) && foreground
	// Approvals happen in the dashboard; without one, nobody could get in.
	if cfg.ask && !interactive {
		return errors.New("--ask needs the dashboard (an interactive terminal); it can't be used headless")
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()
	// The limits watcher can end the session too; its cause is the reason.
	ctx, finish := context.WithCancelCause(sigCtx)
	defer finish(nil)

	store := stats.New()
	store.SetLimit(rates[0])
	store.SetClientLimit(rates[1])
	// Headless mode prints each request as it completes. This must be set
	// before any listener starts serving.
	if !interactive {
		store.OnLog(func(e stats.LogEntry) {
			p, _ := url.PathUnescape(e.Path)
			fmt.Fprintf(stdout, "%s  %-15s %-4s %d  %s  %s  %s\n",
				e.Time.Format("15:04:05"), e.Client, e.Method, e.Status, p,
				humanize.Bytes(e.Bytes), humanize.Duration(e.Duration))
		})
	}
	// Delivery receipts can go to a file as well as the dashboard.
	if cfg.receipts != "" {
		write, closeFile, err := receiptsWriter(cfg.receipts)
		if err != nil {
			return fmt.Errorf("--receipts: %w", err)
		}
		defer closeFile()
		store.OnReceipt(write)
	}
	go store.Run(ctx)

	reg := devices.New(cfg.ask, devices.TrustFile(brand.Command))
	var mgr *listeners.Manager
	var responder *mdns.Responder // set once the server is announced
	selfID := func() string {
		if responder == nil {
			return ""
		}
		return responder.ID()
	}
	scfg := server.Config{
		Paths: paths, ShowHidden: cfg.hidden, Token: token, NoThumbs: cfg.noThumbs,
		BasicUser: user, BasicPass: pass, Store: store, Assets: web.Assets(),
		Devices: reg, Inbox: cfg.inbox, InboxMax: inboxMax, InboxSort: cfg.inboxSort,
		MaxBytes: lim.MaxBytes, Expires: lim.Deadline, Once: lim.Once,
		TV: cfg.tv, TVName: cfg.tvName, TVSecretFile: server.TVSecretFile(brand.Command),
		// Share links point at the address other devices can reach.
		BaseURL: func() string { return listeners.ReachableURL(mgr.List()) },
	}
	if !cfg.noMDNS {
		scfg.Nearby = nearby(selfID)
		scfg.Flock = cfg.flock
	}
	// Headless, messages from devices are printed like requests.
	if !interactive {
		scfg.OnPost = func(m server.Message) {
			fmt.Fprintf(stdout, "%s  message from %s: %s\n", m.Time.Format("15:04:05"), m.From, m.Text)
		}
	}
	srv, err := server.New(scfg)
	if err != nil {
		return err
	}
	defer srv.Close()

	mgr = listeners.NewManager(listeners.Discover(), srv, cfg.port, store)
	ids, err := selectListeners(mgr, cfg.bind)
	if err != nil {
		return err
	}
	// HTTPS: every listener gets the certificate.
	fingerprint := ""
	if cfg.tls != "" {
		tlsCfg, fp, err := certs.Load(cfg.tls, certs.Dir(brand.Command), machineAddrs(mgr))
		if err != nil {
			return fmt.Errorf("--tls: %w", err)
		}
		mgr.SetTLS(tlsCfg)
		fingerprint = fp
	}
	// Bind before anything is drawn: the splash never delays serving.
	port, err := mgr.Start(ids)
	if err != nil {
		reportBindErrors(stderr, mgr.List())
		return fmt.Errorf("could not start: %w", err)
	}
	if port != cfg.port {
		fmt.Fprintf(stdout, "port %d is busy, using %d instead\n", cfg.port, port)
	}

	// Announce the server (and TV mode) on the network. Neither is fatal.
	var notes netNotes
	if !cfg.noMDNS {
		r, inst, err := announce(srv, mgr, authMode(token, user), fingerprint)
		if err != nil {
			notes.mdnsErr = err.Error()
		} else {
			responder, notes.mdnsName = r, inst
			defer r.Close()
		}
	}
	if cfg.tv {
		a, err := advertiseTV(srv, mgr)
		if err != nil {
			notes.tvErr = err.Error()
		} else {
			notes.tvName = srv.TVName()
			defer a.Close()
		}
	}
	notes.fingerprint, notes.receipts, notes.inboxSort = fingerprint, cfg.receipts, cfg.inboxSort
	notes.flock = cfg.flock

	infos := mgr.List()
	urls := printBanner(stdout, dir, infos, token, authMode(token, user))
	printExtras(stdout, srv.Inbox(), inboxMax, cfg.ask, lim, rates)
	printNetwork(stdout, notes)
	reportBindErrors(stderr, infos)
	go watchLimits(ctx, finish, store, lim)

	// Run the dashboard, or just wait for a signal in headless mode.
	if interactive {
		splash := !cfg.noSplash && os.Getenv("NO_COLOR") == ""
		err = tui.Run(tui.Options{
			Ctx: ctx, Store: store, Manager: mgr, Dir: dir, Roots: paths, Token: token,
			AuthMode: authMode(token, user), Splash: splash, URLs: urls,
			Server: srv, Devices: reg, Limits: lim,
			Fingerprint: fingerprint, MDNSName: notes.mdnsName, TVName: notes.tvName,
		})
	} else {
		// Ctrl+C only reaches a process that owns the terminal.
		if foreground {
			fmt.Fprintln(stdout, "press Ctrl+C to stop")
		} else {
			fmt.Fprintf(stdout, "running headless as pid %d; send SIGINT or SIGTERM to stop\n", os.Getpid())
		}
		<-ctx.Done()
	}

	// Graceful shutdown: end SSE streams, stop accepting, give in-flight
	// transfers a short grace period, then close what's left.
	if cause := context.Cause(ctx); errors.Is(cause, errExpired) || errors.Is(cause, errOnce) || errors.Is(cause, errBudget) {
		fmt.Fprintf(stdout, "stopping: %v\n", cause)
	}
	fmt.Fprintln(stdout, "shutting down…")
	srv.Close()
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if mgr.Shutdown(sctx) != nil {
		fmt.Fprintln(stdout, "some transfers were still running and have been cancelled")
	}
	snap := store.Snapshot()
	fmt.Fprintf(stdout, "served %s in %d requests (%d downloads) over %s\n",
		humanize.Bytes(snap.BytesServed), snap.Requests, snap.Downloads, humanize.Duration(snap.Uptime))
	if snap.Uploads > 0 {
		fmt.Fprintf(stdout, "received %d upload(s), %s, in %s\n", snap.Uploads, humanize.Bytes(snap.BytesRecv), srv.Inbox())
	}
	return err
}

// parseRates reads --limit and --client-limit (bytes per second; 0 = none).
func parseRates(cfg config) ([2]int64, error) {
	var out [2]int64
	// Same rules for both flags.
	for i, f := range []struct{ name, val string }{{"--limit", cfg.limit}, {"--client-limit", cfg.clientLimit}} {
		if f.val == "" {
			continue
		}
		n, err := humanize.ParseRate(f.val)
		if err != nil || n <= 0 {
			return out, fmt.Errorf("%s: %q is not a positive rate (try 5M)", f.name, f.val)
		}
		out[i] = n
	}
	return out, nil
}

// describePaths names what is served: the directory, or the paths
// comma-separated when there are several.
func describePaths(paths []string) string {
	return strings.Join(paths, ", ")
}

// parseLimits reads --expire, --once and --max-bytes.
func parseLimits(cfg config) (tui.Limits, error) {
	var lim tui.Limits
	if cfg.expire < 0 {
		return lim, errors.New("--expire must be positive")
	}
	if cfg.expire > 0 {
		lim.Deadline = time.Now().Add(cfg.expire)
	}
	lim.Once = cfg.once
	if cfg.maxBytes != "" {
		n, err := humanize.ParseBytes(cfg.maxBytes)
		if err != nil || n <= 0 {
			return lim, fmt.Errorf("--max-bytes: %q is not a positive size (try 5G)", cfg.maxBytes)
		}
		lim.MaxBytes = n
	}
	return lim, nil
}

// watchLimits ends the session (via finish) when a limit is reached:
// --expire at its deadline; --once after the first completed download, once
// nothing else is still downloading; --max-bytes once that much has been
// served (the server refuses new downloads from then on) and the running
// ones have finished.
func watchLimits(ctx context.Context, finish context.CancelCauseFunc, st *stats.Store, lim tui.Limits) {
	if !lim.Active() {
		return
	}
	tk := time.NewTicker(250 * time.Millisecond)
	defer tk.Stop()
	// Check a few times a second until a limit trips or the session ends.
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		idle := st.ActiveDownloads() == 0
		switch {
		case !lim.Deadline.IsZero() && !time.Now().Before(lim.Deadline):
			finish(errExpired)
			return
		case lim.Once && idle && st.Snapshot().Downloads > 0:
			finish(errOnce)
			return
		case lim.MaxBytes > 0 && idle && st.BytesServed() >= lim.MaxBytes:
			finish(errBudget)
			return
		}
	}
}

// printExtras lists the optional features in the startup banner.
func printExtras(w io.Writer, inbox string, inboxMax int64, ask bool, lim tui.Limits, rates [2]int64) {
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color("#8b949e"))
	bold := lipgloss.NewStyle().Bold(true)
	printed := false
	if inbox != "" {
		note := "  (visitors can upload here"
		if inboxMax > 0 {
			note += ", up to " + humanize.Bytes(inboxMax) + " per file"
		}
		lipgloss.Fprintln(w, "  inbox: "+bold.Render(inbox)+muted.Render(note+")"))
		printed = true
	}
	if ask {
		lipgloss.Fprintln(w, "  "+bold.Render("--ask")+muted.Render(": each new device waits until you allow it in the dashboard"))
		printed = true
	}
	if s := lim.Describe(); s != "" {
		lipgloss.Fprintln(w, "  stops "+bold.Render(s))
		printed = true
	}
	if rates[0] > 0 || rates[1] > 0 {
		var parts []string
		if rates[0] > 0 {
			parts = append(parts, humanize.Rate(float64(rates[0]))+" total")
		}
		if rates[1] > 0 {
			parts = append(parts, humanize.Rate(float64(rates[1]))+" per device")
		}
		lipgloss.Fprintln(w, "  speed limit: "+bold.Render(strings.Join(parts, ", "))+muted.Render("  (change it from the dashboard's esc menu)"))
		printed = true
	}
	if printed {
		lipgloss.Fprintln(w)
	}
}

// netNotes are the network features to mention in the startup banner.
type netNotes struct {
	fingerprint string // HTTPS certificate, lowercase hex
	mdnsName    string // name the server is announced under
	mdnsErr     string
	tvName      string // TV mode's name
	tvErr       string
	receipts    string // receipts file
	inboxSort   string
	flock       bool // nearby servers open in the web UI
}

// printNetwork lists HTTPS, discovery, TV mode and receipts in the banner.
func printNetwork(w io.Writer, n netNotes) {
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color("#8b949e"))
	bold := lipgloss.NewStyle().Bold(true)
	warn := lipgloss.NewStyle().Foreground(lipgloss.Color("#d29922"))
	printed := false
	if n.fingerprint != "" {
		lipgloss.Fprintln(w, "  https certificate SHA-256: "+bold.Render(certs.Pretty(n.fingerprint, 0)))
		lipgloss.Fprintln(w, muted.Render("    browsers warn once about a self-signed certificate: check that the fingerprint they show starts like this"))
		printed = true
	}
	if n.inboxSort == "date" {
		lipgloss.Fprintln(w, "  inbox sorted "+bold.Render("by date")+muted.Render("  (YYYY/MM folders; copies of files already there are skipped)"))
		printed = true
	}
	switch {
	case n.mdnsName != "":
		lipgloss.Fprintln(w, "  on the network as "+bold.Render(n.mdnsName)+muted.Render("  ("+brand.Command+" find, "+brand.Command+" pull)"))
		printed = true
	case n.mdnsErr != "":
		lipgloss.Fprintln(w, warn.Render("  not announced on the network: "+n.mdnsErr))
		printed = true
	}
	if n.flock {
		lipgloss.Fprintln(w, "  flock: "+bold.Render("nearby servers open in the web UI")+muted.Render("  (they see the requests as coming from this computer)"))
		printed = true
	}
	switch {
	case n.tvName != "":
		lipgloss.Fprintln(w, "  TV mode: "+bold.Render(n.tvName)+muted.Render("  (media files are playable by any TV or player on the network)"))
		printed = true
	case n.tvErr != "":
		lipgloss.Fprintln(w, warn.Render("  TV mode is off: "+n.tvErr))
		printed = true
	}
	if n.receipts != "" {
		lipgloss.Fprintln(w, "  delivery receipts: "+bold.Render(n.receipts))
		printed = true
	}
	if printed {
		lipgloss.Fprintln(w)
	}
}

// selectListeners turns the --bind value into listener IDs.
func selectListeners(mgr *listeners.Manager, spec string) ([]string, error) {
	infos := mgr.List()
	byKind := func(k listeners.Kind) []string {
		var ids []string
		// Collect every listener of the requested kind.
		for _, l := range infos {
			if l.Kind == k {
				ids = append(ids, l.ID)
			}
		}
		return ids
	}
	var ids []string
	// Each comma-separated term adds one group or one address.
	for term := range strings.SplitSeq(spec, ",") {
		term = strings.ToLower(strings.TrimSpace(term))
		switch term {
		case "":
		case "default":
			ids = append(ids, byKind(listeners.KindLoopback)...)
			ids = append(ids, byKind(listeners.KindLAN)...)
		case "loopback", "local", "localhost":
			ids = append(ids, byKind(listeners.KindLoopback)...)
		case "lan":
			ids = append(ids, byKind(listeners.KindLAN)...)
		case "vpn", "tailscale":
			ids = append(ids, byKind(listeners.KindVPN)...)
		case "virtual":
			ids = append(ids, byKind(listeners.KindVirtual)...)
		case "all", "*", "0.0.0.0", "::":
			ids = append(ids, listeners.WildcardID)
		default:
			ip, err := netip.ParseAddr(strings.Trim(term, "[]"))
			if err != nil {
				return nil, fmt.Errorf("--bind: %q is not a listener group or IP address", term)
			}
			// Unknown addresses are still attempted so the bind error is clear.
			ids = append(ids, mgr.AddCustom(ip.Unmap()))
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("--bind %q matched no addresses on this machine", spec)
	}
	return ids, nil
}

// printBanner prints the served directory and every URL before the TUI
// takes the screen, so they remain in the scrollback after exit. It returns
// the reachable URLs for the splash to fade in.
func printBanner(w io.Writer, dir string, infos []listeners.Info, token, auth string) []string {
	accent := lipgloss.NewStyle().Foreground(lipgloss.Color("#3dd6c6")).Bold(true)
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color("#8b949e"))
	bold := lipgloss.NewStyle().Bold(true)

	lipgloss.Fprintln(w)
	lipgloss.Fprintln(w, "  "+accent.Render("⬡ "+brand.Name)+muted.Render(" "+brand.ResolvedVersion())+"  serving "+bold.Render(dir))
	lipgloss.Fprintln(w)

	var urls []string
	suffix := ""
	if token != "" {
		suffix = "?token=" + url.QueryEscape(token)
	}
	// One line per listener; disabled ones are listed dimmed for reference.
	for _, l := range infos {
		label := kindLabel(l)
		if !l.Serving() || l.Covered {
			// Covered addresses are reachable, through the wildcard.
			if l.Covered {
				u := l.URL() + suffix
				urls = append(urls, u)
				lipgloss.Fprintln(w, "    ➜  "+padLabel(label)+accent.Render(u))
			} else {
				lipgloss.Fprintln(w, muted.Render("    ·  "+padLabel(label)+strings.TrimSuffix(l.URL(), "/")+"  (off)"))
			}
			continue
		}
		u := l.URL() + suffix
		// "http://0.0.0.0" isn't openable; point to the real addresses instead.
		if l.ID == listeners.WildcardID {
			lipgloss.Fprintln(w, "    ➜  "+padLabel(label)+accent.Render(u)+muted.Render("  (every interface)"))
			continue
		}
		urls = append(urls, u)
		lipgloss.Fprintln(w, "    ➜  "+padLabel(label)+accent.Render(u))
	}
	lipgloss.Fprintln(w)
	switch {
	case token != "":
		lipgloss.Fprintln(w, "  access token: "+bold.Render(token)+muted.Render("  (included in the links above)"))
	case auth == "open":
		lipgloss.Fprintln(w, muted.Render("  no authentication: anyone who can reach these addresses can download (see --token)"))
	default:
		lipgloss.Fprintln(w, muted.Render("  basic auth required"))
	}
	lipgloss.Fprintln(w)
	return urls
}

// kindLabel names a listener for the banner.
func kindLabel(l listeners.Info) string {
	switch l.Kind {
	case listeners.KindLoopback:
		return "Local"
	case listeners.KindLAN:
		if l.Primary {
			return "Network"
		}
		return "LAN (" + l.Iface + ")"
	case listeners.KindVPN:
		return "VPN (" + l.Iface + ")"
	case listeners.KindVirtual:
		return "Virtual (" + l.Iface + ")"
	case listeners.KindWildcard:
		return "All"
	}
	return "Custom"
}

// padLabel pads a banner label to a fixed column.
func padLabel(s string) string {
	return s + ":" + strings.Repeat(" ", max(1, 18-len(s)))
}

// reportBindErrors prints any per-listener bind failures.
func reportBindErrors(w io.Writer, infos []listeners.Info) {
	// Only listeners with a recorded error are reported.
	for _, l := range infos {
		if l.Err != "" {
			fmt.Fprintf(w, "  warning: %s\n", l.Err)
		}
	}
}

// authMode summarises the access gate for display.
func authMode(token, user string) string {
	switch {
	case token != "" && user != "":
		return "token + basic"
	case token != "":
		return "token"
	case user != "":
		return "basic"
	}
	return "open"
}

// shutdownSignals lists the signals that trigger a graceful shutdown.
// SIGHUP (terminal closed) is included unless it was ignored at startup, as
// under nohup: subscribing to it would undo that and kill the server on logout.
func shutdownSignals() []os.Signal {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return sigs
}

// isTTY reports whether f is an interactive terminal.
func isTTY(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// randomToken generates a short, URL-safe access token.
func randomToken() string {
	b := make([]byte, 9)
	rand.Read(b)
	return hex.EncodeToString(b)
}
