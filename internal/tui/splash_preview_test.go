package tui

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"testing/fstest"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Indigo6415/pidgeon/internal/devices"
	"github.com/Indigo6415/pidgeon/internal/listeners"
	"github.com/Indigo6415/pidgeon/internal/server"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// TestSplashPreview prints a few splash frames when PIDGEON_PREVIEW=1, so
// the art can be checked without launching the full program.
func TestSplashPreview(t *testing.T) {
	if os.Getenv("PIDGEON_PREVIEW") == "" {
		t.Skip("set PIDGEON_PREVIEW=1 to print splash frames")
	}
	th := newTheme(true, hex("#0d1117"))
	s := splash{start: time.Unix(0, 0), urls: []string{"http://127.0.0.1:8080/", "http://192.168.1.20:8080/"}}
	// Render a frame at each interesting moment of the timeline.
	for _, ms := range []int{0, 70, 300, 500, 800, 1100} {
		fmt.Printf("--- t=%dms\n%s\n", ms, s.view(time.Unix(0, 0).Add(time.Duration(ms)*time.Millisecond), 60, 22, th))
	}
}

// TestDashboardPreview prints the dashboard with live fake traffic when
// PIDGEON_PREVIEW=1, to eyeball the layout at a typical terminal size.
func TestDashboardPreview(t *testing.T) {
	if os.Getenv("PIDGEON_PREVIEW") == "" {
		t.Skip("set PIDGEON_PREVIEW=1 to print the dashboard")
	}
	st := stats.New()
	mgr := listeners.NewManager(listeners.Discover(), http.NotFoundHandler(), 18200, st)
	if _, err := mgr.Start([]string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	// Fake a few transfers and requests.
	a := st.Begin(stats.TransferInfo{Client: "192.168.1.33", Name: "/movies/Big Buck Bunny.mkv", Total: 1 << 30, Listener: "127.0.0.1"})
	a.Add(700 << 20)
	z := st.Begin(stats.TransferInfo{Client: "192.168.1.40", Name: "/Holiday Photos.zip", Kind: stats.KindZip, Total: -1})
	z.Add(300 << 20)
	d := st.Begin(stats.TransferInfo{Client: "192.168.1.40", Name: "/a.txt", Total: 6})
	d.Add(6)
	st.End(d, stats.Completed)
	for i := range 6 {
		st.Log(stats.LogEntry{Time: time.Now(), Client: "192.168.1.33", Method: "GET", Path: fmt.Sprintf("/f/file%d.jpg", i), Status: 200 + i%3*102, Bytes: int64(i) << 20})
	}
	// Devices, a share link, an upload and a time limit, so the newer
	// panels show up too.
	reg := devices.New(true, "")
	reg.Allow("192.168.1.33", false)
	reg.Admit(context.Background(), "192.168.1.33", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0) Safari/604.1", 0)
	reg.Allow("192.168.1.40", true)
	reg.Admit(context.Background(), "192.168.1.40", "Mozilla/5.0 (Macintosh) Chrome/130.0 Safari/537.36", 0)
	srv, err := server.New(server.Config{Dir: t.TempDir(), Store: st, Devices: reg, Assets: fstest.MapFS{"index.html": {Data: []byte("x")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.CreateShare("/", time.Hour, 3)
	u := st.Begin(stats.TransferInfo{Client: "192.168.1.33", Name: "/Inbox/IMG_2041.HEIC", Kind: stats.KindUpload, Total: 4 << 20})
	u.Add(3 << 20)
	dash := newDashboard(Options{Store: st, Manager: mgr, Dir: "/Users/someone/Downloads", AuthMode: "open",
		Server: srv, Devices: reg, Limits: Limits{Deadline: time.Now().Add(29 * time.Minute)}})
	dash.th = newTheme(true, hex("#0d1117"))
	fmt.Println(dash.view(130, 46))
}

// TestMenuPreview prints the dashboard with the esc menu open.
func TestMenuPreview(t *testing.T) {
	if os.Getenv("PIDGEON_PREVIEW") == "" {
		t.Skip("set PIDGEON_PREVIEW=1 to print the menu")
	}
	st := stats.New()
	mgr := listeners.NewManager(listeners.Discover(), http.NotFoundHandler(), 18210, st)
	mgr.Start([]string{"127.0.0.1"})
	defer mgr.Shutdown(context.Background())
	dash := newDashboard(Options{Store: st, Manager: mgr, Dir: "/srv", AuthMode: "open"})
	dash.handleKey(tea.KeyPressMsg{Code: tea.KeyRight})
	fmt.Println(dash.view(110, 30))
}
