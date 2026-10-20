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
	"github.com/Indigo6415/pidgeon/internal/humanize"
	"github.com/Indigo6415/pidgeon/internal/listeners"
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
	dir      string
	port     int
	bind     string
	token    string
	auth     string
	hidden   bool
	noSplash bool
	headless bool
	version  bool
}

// Main runs the program with the given arguments (excluding argv[0]) and
// returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	// Subcommands come first; everything else is the server.
	if len(args) > 0 && args[0] == "pull" {
		return pull.Main(args[1:], stdout, stderr)
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
	fs.Usage = func() {
		fmt.Fprintf(stderr, "%s — %s\n\n", brand.Name, brand.Tagline)
		fmt.Fprintf(stderr, "Usage:\n  %s [flags] [directory]\n  %s pull <url> [target-dir]   (preview)\n\n", brand.Command, brand.Command)
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
	switch fs.NArg() {
	case 0:
		c.dir = "."
	case 1:
		c.dir = fs.Arg(0)
	default:
		fmt.Fprintln(stderr, "too many arguments: expected at most one directory")
		return c, 2
	}
	return c, -1
}

// serve runs the server until the user quits or a signal arrives.
func serve(cfg config, stdout, stderr io.Writer) error {
	dir, err := filepath.Abs(cfg.dir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", cfg.dir)
	}

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

	// The dashboard needs a terminal on both ends that this process owns;
	// anything else (pipes, services, containers, `&`) runs headless.
	foreground := isTTY(os.Stdin) && inForeground(os.Stdin)
	interactive := !cfg.headless && isTTY(os.Stdout) && foreground

	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()

	store := stats.New()
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
	go store.Run(ctx)

	srv, err := server.New(server.Config{
		Dir: dir, ShowHidden: cfg.hidden, Token: token,
		BasicUser: user, BasicPass: pass, Store: store, Assets: web.Assets(),
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	mgr := listeners.NewManager(listeners.Discover(), srv, cfg.port, store)
	ids, err := selectListeners(mgr, cfg.bind)
	if err != nil {
		return err
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

	infos := mgr.List()
	urls := printBanner(stdout, dir, infos, token, authMode(token, user))
	reportBindErrors(stderr, infos)

	// Run the dashboard, or just wait for a signal in headless mode.
	if interactive {
		splash := !cfg.noSplash && os.Getenv("NO_COLOR") == ""
		err = tui.Run(tui.Options{
			Ctx: ctx, Store: store, Manager: mgr, Dir: dir, Token: token,
			AuthMode: authMode(token, user), Splash: splash, URLs: urls,
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
	return err
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
