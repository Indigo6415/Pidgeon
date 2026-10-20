package tui

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Indigo6415/pidgeon/internal/listeners"
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
	dash := newDashboard(Options{Store: st, Manager: mgr, Dir: "/Users/someone/Downloads", AuthMode: "open"})
	dash.th = newTheme(true, hex("#0d1117"))
	fmt.Println(dash.view(130, 42))
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
