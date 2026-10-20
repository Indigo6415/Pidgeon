package tui

import (
	"context"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Indigo6415/pidgeon/internal/devices"
	"github.com/Indigo6415/pidgeon/internal/listeners"
	"github.com/Indigo6415/pidgeon/internal/server"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// newPeopleDash builds a dashboard with a real server and device registry
// over a tiny tree (a.txt, docs/b.txt).
func newPeopleDash(t *testing.T, ask bool) (*dashboard, *devices.Registry, *server.Server) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.WriteFile(filepath.Join(dir, "docs", "b.txt"), []byte("b"), 0o644)
	st := stats.New()
	reg := devices.New(ask, "")
	srv, err := server.New(server.Config{Dir: dir, Store: st, Devices: reg, Assets: fstest.MapFS{"index.html": {Data: []byte("x")}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	lo := listeners.Candidate{ID: "127.0.0.1", Addr: netip.MustParseAddr("127.0.0.1"), Kind: listeners.KindLoopback}
	mgr := listeners.NewManager([]listeners.Candidate{lo}, http.NotFoundHandler(), 18770, st)
	if _, err := mgr.Start([]string{lo.ID}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })
	d := newDashboard(Options{Store: st, Manager: mgr, Dir: dir, AuthMode: "open", Server: srv, Devices: reg})
	return d, reg, srv
}

// TestApprovalPromptAnswersWithOneKey: a new device in ask mode pops a
// prompt that "y" answers; esc puts it off instead.
func TestApprovalPromptAnswersWithOneKey(t *testing.T) {
	d, reg, _ := newPeopleDash(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Two devices knock, in this order; each waits for an answer.
	waitPrompts := func(n int) {
		for range 200 {
			if len(reg.Prompts()) == n {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	go reg.Admit(ctx, "10.0.0.7", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0) Safari/604.1", time.Minute)
	waitPrompts(1)
	go reg.Admit(ctx, "10.0.0.8", "curl/8.0", time.Minute)
	waitPrompts(2)
	d.refresh()
	if d.menu == nil || d.menu.prompt == 0 || !strings.Contains(strings.Join(d.menu.note, " "), "iPhone · Safari") {
		t.Fatalf("expected a prompt for the iPhone, got %+v", d.menu)
	}
	press(d, 'y')
	if st := reg.Status("10.0.0.7"); st != devices.Allowed {
		t.Fatalf("y should allow, status %v", st)
	}
	// The second device's prompt follows; esc defers it without deciding.
	if d.menu == nil || d.menu.prompt == 0 {
		t.Fatal("the next prompt should open")
	}
	press(d, tea.KeyEscape)
	if d.menu != nil || reg.Status("10.0.0.8") != devices.Pending {
		t.Fatal("esc should put the prompt off, not answer it")
	}
	if out := d.view(120, 40); !strings.Contains(out, "⚑ 1 waiting") || !strings.Contains(out, "Devices") {
		t.Fatal("header should flag the waiting device and the Devices panel should show")
	}
}

// TestCursorFlowsThroughDevicesAndCreatesShares: ↓ walks listeners →
// devices → shares; "s" builds a share link through the picker.
func TestCursorFlowsThroughDevicesAndCreatesShares(t *testing.T) {
	d, reg, srv := newPeopleDash(t, false)
	reg.Admit(context.Background(), "10.0.0.7", "Mozilla/5.0 (Android 15) Chrome/130", 0)
	d.refresh()
	press(d, tea.KeyDown)
	if d.focus != focusDevices || d.dsel != "10.0.0.7" {
		t.Fatalf("↓ should enter the devices panel (focus %v sel %q)", d.focus, d.dsel)
	}
	press(d, tea.KeyEnter)
	if d.menu == nil || !strings.HasPrefix(d.menu.title, "Android · Chrome") {
		t.Fatalf("enter should open the device's actions, got %+v", d.menu)
	}
	press(d, tea.KeyEscape)

	// s → picker; "★ Share this folder" is first, then "docs/", then a.txt.
	press(d, 's')
	if d.menu == nil || d.menu.back == nil {
		t.Fatal("s should open the file picker")
	}
	press(d, tea.KeyDown, tea.KeyDown, tea.KeyEnter) // a.txt
	if d.menu == nil || d.menu.title != "Link expires" {
		t.Fatalf("picking a file should ask for the expiry, got %+v", d.menu)
	}
	press(d, tea.KeyEnter, tea.KeyEnter) // 1 hour, 1 download
	sh := srv.Shares()
	if len(sh) != 1 || sh[0].Path != "/a.txt" || sh[0].MaxUses != 1 || sh[0].Expires.IsZero() {
		t.Fatalf("shares = %+v", sh)
	}
	if d.focus != focusShares || d.ssel != sh[0].ID || !strings.Contains(d.qr.text, "/s/"+sh[0].ID) {
		t.Fatal("the new link should be selected and on the QR code")
	}
	// ← in the picker goes up a folder rather than closing.
	press(d, 's', tea.KeyDown, tea.KeyEnter) // into docs/
	if !strings.HasSuffix(d.menu.title, "/docs") {
		t.Fatalf("expected the docs folder, got %q", d.menu.title)
	}
	press(d, tea.KeyLeft)
	if d.menu == nil || !strings.HasSuffix(d.menu.title, "· /") {
		t.Fatalf("← should go up, got %+v", d.menu)
	}
}

func TestNicknames(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Version/18.0 Mobile/15E148 Safari/604.1": "iPhone · Safari",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/130.0 Safari/537.36":                       "Mac · Chrome",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130.0 Safari/537.36 Edg/130.0":                   "Windows · Edge",
		"Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0":                                              "Linux · Firefox",
		"curl/8.7.1": "curl",
	}
	// Each User-Agent maps to its short label.
	for ua, want := range cases {
		if got := devices.Nickname(ua); got != want {
			t.Errorf("%q → %q, want %q", ua, got, want)
		}
	}
}
