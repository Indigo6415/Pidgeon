package tui

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Indigo6415/pidgeon/internal/listeners"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// press sends a sequence of special keys to the dashboard and returns the
// command produced by the last one.
func press(d *dashboard, codes ...rune) tea.Cmd {
	var cmd tea.Cmd
	// Each key is delivered exactly as Bubble Tea would.
	for _, c := range codes {
		cmd = d.handleKey(tea.KeyPressMsg{Code: c})
		d.refresh()
	}
	return cmd
}

// isQuit reports whether cmd is tea.Quit.
func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// TestArrowsEnterEscReachEverything drives every dashboard action with only
// ↑ ↓ → enter and esc.
func TestArrowsEnterEscReachEverything(t *testing.T) {
	st := stats.New()
	lo := listeners.Candidate{ID: "127.0.0.1", Addr: netip.MustParseAddr("127.0.0.1"), Kind: listeners.KindLoopback}
	wild := listeners.Candidate{ID: listeners.WildcardID, Kind: listeners.KindWildcard}
	mgr := listeners.NewManager([]listeners.Candidate{lo, wild}, http.NotFoundHandler(), 18760, st)
	if _, err := mgr.Start([]string{lo.ID}); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	tr := st.Begin(stats.TransferInfo{Client: "10.0.0.2", Name: "/big.iso", Total: 100})

	d := newDashboard(Options{Store: st, Manager: mgr, Dir: "/tmp", AuthMode: "open"})

	// enter toggles the listener under the cursor (loopback, row 0).
	press(d, tea.KeyEnter)
	if d.lis[0].Enabled {
		t.Fatal("enter did not disable the loopback listener")
	}
	press(d, tea.KeyEnter)
	if !d.lis[0].Enabled {
		t.Fatal("enter did not re-enable the loopback listener")
	}

	// → opens the listener's actions; enter on the first item toggles too.
	press(d, tea.KeyRight)
	if d.menu == nil || !strings.HasPrefix(d.menu.items[0].label, "Disable") {
		t.Fatalf("→ should open listener actions, got %+v", d.menu)
	}
	press(d, tea.KeyEscape)
	if d.menu != nil {
		t.Fatal("esc should close the actions menu")
	}

	// ↓ past the last listener lands on the transfer; its menu starts with
	// pause (enter → enter pauses, again resumes), then limit, then cancel.
	press(d, tea.KeyDown, tea.KeyDown)
	if d.focus != focusTransfers || d.tsel != tr.ID {
		t.Fatalf("↓ should flow into transfers (focus=%v sel=%d)", d.focus, d.tsel)
	}
	press(d, tea.KeyEnter, tea.KeyEnter)
	if !d.snap.Transfers[0].Paused {
		t.Fatal("enter, enter on a transfer should pause it")
	}
	press(d, tea.KeyEnter, tea.KeyEnter)
	if d.snap.Transfers[0].Paused {
		t.Fatal("enter, enter on a paused transfer should resume it")
	}
	press(d, tea.KeyEnter, tea.KeyDown, tea.KeyDown, tea.KeyEnter)
	if !tr.Cancelled() {
		t.Fatal("enter, ↓, ↓, enter on a transfer should cancel it")
	}

	// ↑ climbs back into the listeners panel.
	press(d, tea.KeyUp)
	if d.focus != focusListeners || d.lcur != 1 {
		t.Fatalf("↑ should return to the last listener (focus=%v cur=%d)", d.focus, d.lcur)
	}

	// esc opens the main menu: "all keys" is item 2, bandwidth item 3, quit item 4.
	press(d, tea.KeyEscape, tea.KeyDown, tea.KeyEnter)
	if !d.help.ShowAll {
		t.Fatal("main menu should toggle the full key list")
	}
	press(d, tea.KeyEscape) // collapses the key list
	if d.help.ShowAll || d.menu != nil {
		t.Fatal("esc should first collapse the key list")
	}
	if !isQuit(press(d, tea.KeyEscape, tea.KeyDown, tea.KeyDown, tea.KeyDown, tea.KeyEnter)) {
		t.Fatal("esc → quit → enter should quit")
	}

	// The menu renders on top of the dashboard without breaking the layout.
	press(d, tea.KeyEscape)
	if out := d.view(100, 30); !strings.Contains(out, "Quit Pidgeon") || strings.Count(out, "\n") != 29 {
		t.Fatalf("menu overlay missing or layout height wrong (%d lines)", strings.Count(out, "\n")+1)
	}
}
