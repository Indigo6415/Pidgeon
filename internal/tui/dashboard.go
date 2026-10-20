package tui

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Indigo6415/pidgeon/internal/brand"
	"github.com/Indigo6415/pidgeon/internal/humanize"
	"github.com/Indigo6415/pidgeon/internal/listeners"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// focus is which panel the cursor keys act on.
type focus int

const (
	focusListeners focus = iota
	focusTransfers
)

// spinner frames for transfers whose size isn't known (ZIP streams).
var spinner = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// dashboard holds the dashboard's UI state. It is a pointer inside the root
// model so key handlers can mutate it in place.
type dashboard struct {
	o     Options
	th    theme
	keys  keyMap
	help  help.Model
	focus focus

	lcur int    // listener cursor
	tsel uint64 // selected transfer ID (stable while the list reshuffles)

	snap  stats.Snapshot
	lis   []listeners.Info
	frame int

	flash      string
	flashErr   bool
	flashUntil time.Time
	quitArmed  time.Time

	qr   qrBlock
	qrOK bool

	menu *menu // open popup menu, if any; it captures all keys
}

// newDashboard creates the dashboard and takes a first snapshot.
func newDashboard(o Options) *dashboard {
	h := help.New()
	h.Styles = help.DefaultStyles(true)
	d := &dashboard{o: o, th: newTheme(true, hex("#000000")), keys: defaultKeys(), help: h}
	d.refresh()
	return d
}

// refresh pulls fresh data from the stats store and listener manager.
func (d *dashboard) refresh() {
	d.snap = d.o.Store.Snapshot()
	d.lis = d.o.Manager.List()
	d.frame++
	d.lcur = min(max(0, d.lcur), max(0, len(d.lis)-1))
	// Transfers expire after a minute; don't leave focus on an empty panel.
	if d.focus == focusTransfers && len(d.snap.Transfers) == 0 {
		d.focus = focusListeners
	}

	// Re-encode the QR code only when its target URL changes.
	if u := d.shareURL(); u != d.qr.text {
		d.qr, d.qrOK = renderQR(u)
		d.qr.text = u
	}
}

// say shows a transient message in the footer.
func (d *dashboard) say(msg string, isErr bool) {
	d.flash, d.flashErr, d.flashUntil = msg, isErr, time.Now().Add(4*time.Second)
}

// activeCount returns the number of in-flight transfers.
func (d *dashboard) activeCount() int {
	n := 0
	// Active transfers are sorted first, so stop at the first finished one.
	for _, t := range d.snap.Transfers {
		if t.State != stats.StateActive {
			break
		}
		n++
	}
	return n
}

// handleKey applies one key press and returns any command to run.
//
// Everything works with the arrows, enter and esc alone:
//
//	↑/↓    move through listeners, then on into transfers
//	enter  toggle the selected listener / open the selected transfer's actions
//	→      actions for the selected row (toggle, copy URL, open, cancel)
//	esc    main menu (all keys, quit); closes any open menu
//
// The letter keys (space, tab, x, c, o, ?, q) are shortcuts for the same.
func (d *dashboard) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	k := d.keys
	// ctrl+c always quits, even from inside a menu.
	if msg.String() == "ctrl+c" {
		return tea.Quit
	}
	// An open menu captures every key until it closes.
	if d.menu != nil {
		return d.menuKey(msg)
	}
	switch {
	case key.Matches(msg, k.Menu):
		// Esc first collapses the full key list, then opens the main menu.
		if d.help.ShowAll {
			d.help.ShowAll = false
			return nil
		}
		d.menu = d.mainMenu()

	case key.Matches(msg, k.Up):
		d.move(-1)
	case key.Matches(msg, k.Down):
		d.move(1)

	case key.Matches(msg, k.Enter):
		// Listeners toggle directly; transfers open their actions.
		if d.focus == focusListeners {
			return d.toggleSelected()
		}
		d.menu = d.rowMenu()

	case key.Matches(msg, k.Actions):
		d.menu = d.rowMenu()

	case key.Matches(msg, k.Toggle):
		return d.toggleSelected()

	case key.Matches(msg, k.Focus):
		// Only move to transfers when there is something to select.
		if d.focus == focusListeners && len(d.snap.Transfers) > 0 {
			d.focus = focusTransfers
			d.ensureTransferSelected()
		} else {
			d.focus = focusListeners
		}

	case key.Matches(msg, k.Cancel):
		if d.focus != focusTransfers {
			d.say("move down to a transfer first, then press enter to cancel it", true)
			return nil
		}
		return d.cancelSelected()

	case key.Matches(msg, k.Copy):
		return d.copyURL()

	case key.Matches(msg, k.Open):
		return d.openURL()

	case key.Matches(msg, k.Help):
		d.help.ShowAll = !d.help.ShowAll

	case key.Matches(msg, k.Quit):
		// Ask for confirmation once if quitting would cut off downloads.
		if n := d.activeCount(); n > 0 && time.Since(d.quitArmed) > 3*time.Second {
			d.quitArmed = time.Now()
			d.say(fmt.Sprintf("%d transfer(s) in progress — press q again to quit", n), true)
			return nil
		}
		return tea.Quit
	}
	return nil
}

// menuKey drives the open popup menu.
func (d *dashboard) menuKey(msg tea.KeyPressMsg) tea.Cmd {
	k := d.keys
	switch {
	case key.Matches(msg, k.Up):
		d.menu.move(-1)
	case key.Matches(msg, k.Down):
		d.menu.move(1)
	case key.Matches(msg, k.Menu), key.Matches(msg, k.Back), msg.String() == "q":
		d.menu = nil
	case key.Matches(msg, k.Enter), key.Matches(msg, k.Actions), msg.String() == "space":
		it, ok := d.menu.selected()
		d.menu = nil
		// Run the action after closing, so it may open another menu.
		if ok && it.run != nil {
			return it.run()
		}
	}
	return nil
}

// mainMenu is what esc opens: help and quit, reachable without letters.
func (d *dashboard) mainMenu() *menu {
	quit := menuItem{label: "Quit " + brand.Name, danger: true, run: func() tea.Cmd { return tea.Quit }}
	// Quitting cancels downloads; say so where the decision is made.
	if n := d.activeCount(); n > 0 {
		quit.hint = fmt.Sprintf("cancels %d active download(s)", n)
	}
	helpLabel := "Show all keys"
	if d.help.ShowAll {
		helpLabel = "Hide all keys"
	}
	return newMenu(brand.Name,
		menuItem{label: "Back to dashboard", run: func() tea.Cmd { return nil }},
		menuItem{label: helpLabel, run: func() tea.Cmd { d.help.ShowAll = !d.help.ShowAll; return nil }},
		quit,
	)
}

// rowMenu lists the actions for the selected listener or transfer.
func (d *dashboard) rowMenu() *menu {
	back := menuItem{label: "Back", run: func() tea.Cmd { return nil }}
	// Transfer row: the only action is cancelling it.
	if d.focus == focusTransfers {
		i := d.selectedIndex()
		if i < 0 {
			return nil
		}
		t := d.snap.Transfers[i]
		cancel := menuItem{label: "Cancel transfer", hint: "the download fails on " + t.Client, danger: true, run: d.cancelSelected}
		if t.State != stats.StateActive {
			cancel.disabled, cancel.hint = true, "already finished"
		}
		return newMenu(t.Name, cancel, back)
	}
	if len(d.lis) == 0 {
		return nil
	}
	l := d.lis[d.lcur]
	toggle := menuItem{label: "Enable listener", run: d.toggleSelected}
	if l.Enabled {
		toggle = menuItem{label: "Disable listener", hint: "running downloads finish", run: d.toggleSelected}
	}
	cp := menuItem{label: "Copy URL", hint: "to the clipboard", run: d.copyURL}
	op := menuItem{label: "Open in browser", run: d.openURL}
	// Copy/open only make sense for an address that is serving.
	if !l.Serving() {
		cp.disabled, cp.hint = true, "enable it first"
		op.disabled = true
	}
	return newMenu("Listener "+listenerAddr(l), toggle, cp, op, back)
}

// toggleSelected enables/disables the listener under the cursor.
func (d *dashboard) toggleSelected() tea.Cmd {
	if len(d.lis) == 0 {
		return nil
	}
	l := d.lis[d.lcur]
	err := d.o.Manager.Toggle(l.ID)
	d.refresh()
	switch {
	case err != nil:
		d.say(err.Error(), true)
	case d.lis[d.lcur].Enabled:
		d.say("enabled "+listenerAddr(d.lis[d.lcur]), false)
	default:
		d.say("disabled "+listenerAddr(l)+" (in-flight downloads finish)", false)
	}
	return nil
}

// cancelSelected cancels the selected transfer.
func (d *dashboard) cancelSelected() tea.Cmd {
	i := d.selectedIndex()
	if i < 0 {
		return nil
	}
	t := d.snap.Transfers[i]
	if d.o.Store.Cancel(t.ID) {
		d.say("cancelled "+t.Name+" → "+t.Client, false)
	} else {
		d.say("that transfer has already finished", true)
	}
	return nil
}

// copyURL puts the share URL on the clipboard (OSC 52).
func (d *dashboard) copyURL() tea.Cmd {
	u := d.shareURL()
	if u == "" {
		return nil
	}
	d.say("copied "+u, false)
	return tea.SetClipboard(u)
}

// openURL opens the share URL in the local browser.
func (d *dashboard) openURL() tea.Cmd {
	u := d.shareURL()
	if u == "" {
		return nil
	}
	d.say("opening "+u, false)
	return openBrowser(u)
}

// move shifts the cursor. The two panels form one list: moving down past
// the last listener enters the transfers, and up past the first transfer
// returns to the listeners.
func (d *dashboard) move(delta int) {
	if d.focus == focusListeners {
		next := d.lcur + delta
		// Fall through into the transfers panel below.
		if next >= len(d.lis) && len(d.snap.Transfers) > 0 {
			d.focus = focusTransfers
			d.tsel = d.snap.Transfers[0].ID
			return
		}
		d.lcur = min(max(0, next), max(0, len(d.lis)-1))
		d.refresh()
		return
	}
	n := len(d.snap.Transfers)
	if n == 0 {
		d.focus = focusListeners
		return
	}
	i := max(0, d.selectedIndex())
	// Climb back up into the listeners panel.
	if i+delta < 0 {
		d.focus = focusListeners
		d.lcur = max(0, len(d.lis)-1)
		d.refresh()
		return
	}
	d.tsel = d.snap.Transfers[min(i+delta, n-1)].ID
}

// ensureTransferSelected selects the first transfer if none is.
func (d *dashboard) ensureTransferSelected() {
	if d.selectedIndex() < 0 && len(d.snap.Transfers) > 0 {
		d.tsel = d.snap.Transfers[0].ID
	}
}

// selectedIndex finds the selected transfer in the current snapshot.
func (d *dashboard) selectedIndex() int {
	// The list reorders as transfers finish, so look the ID up each time.
	for i, t := range d.snap.Transfers {
		if t.ID == d.tsel {
			return i
		}
	}
	return -1
}

// shareURL is the URL used by the QR code, copy and open: the selected
// listener when it is serving, otherwise the primary LAN address. Wildcard
// listeners are translated to a concrete, reachable address.
func (d *dashboard) shareURL() string {
	pick := -1
	if d.lcur < len(d.lis) && d.lis[d.lcur].Serving() {
		pick = d.lcur
	}
	// Fall back to the primary LAN listener, then any serving one.
	for i, l := range d.lis {
		if pick < 0 && l.Primary && l.Serving() {
			pick = i
		}
	}
	for i, l := range d.lis {
		if pick < 0 && l.Serving() {
			pick = i
		}
	}
	if pick < 0 {
		return ""
	}
	l := d.lis[pick]
	host := l.Host()
	// "0.0.0.0" isn't something a phone can open: use a real address.
	if l.ID == listeners.WildcardID {
		host = "127.0.0.1"
		for _, x := range d.lis {
			if x.Primary {
				host = x.Host()
				break
			}
		}
	}
	u := "http://" + host + ":" + strconv.Itoa(l.Port) + "/"
	if d.o.Token != "" {
		u += "?token=" + url.QueryEscape(d.o.Token)
	}
	return u
}

// openBrowser opens u in the default browser without touching the TUI's
// stdout/stderr.
func openBrowser(u string) tea.Cmd {
	return func() tea.Msg {
		var c *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			c = exec.Command("open", u)
		case "windows":
			c = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
		default:
			c = exec.Command("xdg-open", u)
		}
		_ = c.Start()
		return nil
	}
}

// listenerAddr is a listener's "host:port" for display.
func listenerAddr(l listeners.Info) string {
	if l.ID == listeners.WildcardID {
		return "*:" + strconv.Itoa(l.Port)
	}
	return l.Host() + ":" + strconv.Itoa(l.Port)
}

// view lays out the whole dashboard for a w×h terminal.
func (d *dashboard) view(w, h int) string {
	avail := h - 2 // header + footer

	// Decide whether the QR column fits next to the left stack.
	qrW := d.qr.w + 4
	qrH := len(d.qr.lines) + 5 // code + blank + URL + note + borders
	listH := len(d.lis) + 2
	thruBase := 8
	showQR := d.qrOK && w >= qrW+1+56 && avail >= qrH+3
	leftW := w
	if showQR {
		leftW = w - qrW - 1
	}

	// The sparkline grows to keep both top columns the same height.
	sparkRows := 3
	if showQR && qrH > listH+thruBase {
		sparkRows += min(10, qrH-listH-thruBase)
	}
	left := lipgloss.JoinVertical(lipgloss.Left,
		d.listenersPanel(leftW),
		d.throughputPanel(leftW, sparkRows),
	)
	top := left
	if showQR {
		top = lipgloss.JoinHorizontal(lipgloss.Top, left, " ", d.qrPanel(qrW, lipgloss.Height(left)))
	}

	// Transfers get what they need (capped); the request log takes the rest.
	remaining := avail - lipgloss.Height(top)
	sections := []string{d.header(w), top}
	if remaining >= 3 {
		tAvail := remaining - 2
		const logMin = 5
		// Leave room for a useful request log when there's space to spare.
		if tAvail-logMin >= 2 {
			tAvail -= logMin
		}
		rows := min(max(1, len(d.snap.Transfers)), tAvail)
		sections = append(sections, d.transfersPanel(w, rows))
		if logH := remaining - (rows + 2); logH >= 3 {
			sections = append(sections, d.logPanel(w, logH-2))
		}
	}
	// Fit the body to exactly the rows above the footer: clip so the
	// terminal never scrolls, pad so the footer sits on the bottom row and
	// popups centre on the real screen.
	foot := strings.Split(d.footer(w), "\n")
	body := strings.Split(strings.Join(sections, "\n"), "\n")
	bodyH := max(0, h-len(foot))
	if len(body) > bodyH {
		body = body[:bodyH]
	}
	for len(body) < bodyH {
		body = append(body, "")
	}
	out := strings.Join(append(body, foot...), "\n")
	// A popup menu floats over the live dashboard, which keeps updating.
	if d.menu != nil {
		out = overlay(out, d.menu.view(d.th), w, h)
	}
	return out
}

// header is the one-line status bar.
func (d *dashboard) header(w int) string {
	th := d.th
	serving := 0
	for _, l := range d.lis {
		if l.Serving() {
			serving++
		}
	}
	status := fgs(th.ok).Render("● serving")
	if serving == 0 {
		status = fgs(th.err).Render("● stopped")
	}
	leftPart := fgs(th.accent).Bold(true).Render("⬡ "+strings.ToUpper(brand.Name)) +
		fgs(th.muted).Render(" "+brand.ResolvedVersion()) + "  " + status + "  " +
		fgs(th.fg).Render(tildePath(d.o.Dir))
	right := fgs(th.muted).Render(fmt.Sprintf("up %s · auth: %s ", humanize.Duration(d.snap.Uptime), d.o.AuthMode))
	gap := w - lipgloss.Width(leftPart) - lipgloss.Width(right)
	// Not enough room for both halves: keep the left (status + path).
	if gap < 1 {
		return fit(" "+leftPart, w)
	}
	return " " + leftPart + strings.Repeat(" ", max(0, gap-1)) + right
}

// listenersPanel shows every bind address with its state and activity.
func (d *dashboard) listenersPanel(w int) string {
	th := d.th
	addrW := 0
	for _, l := range d.lis {
		addrW = max(addrW, len(listenerAddr(l)))
	}
	var rows []string
	// One row per listener: cursor, checkbox, address, label, status.
	for i, l := range d.lis {
		cur := "  "
		if i == d.lcur {
			c := th.faint
			if d.focus == focusListeners {
				c = th.accent
			}
			cur = fgs(c).Render("▸ ")
		}
		box := fgs(th.muted).Render("[ ]")
		if l.Enabled {
			box = fgs(th.muted).Render("[") + fgs(th.accent).Bold(true).Render("x") + fgs(th.muted).Render("]")
		}
		addrStyle := fgs(th.fg)
		if !l.Serving() {
			addrStyle = fgs(th.muted)
		}
		addr := addrStyle.Render(padRight(listenerAddr(l), addrW))

		label := l.Kind.String()
		if l.Iface != "" {
			label = l.Iface + " · " + label
		}
		if l.ID == listeners.WildcardID {
			label = "all interfaces"
		}
		if l.Primary {
			label += " ★"
		}
		label = fgs(th.muted).Render(padRight(label, 20))

		var st string
		active := d.snap.ActiveByListener[l.ID]
		switch {
		case l.Err != "":
			st = fgs(th.err).Render("! " + l.Err)
		case l.Covered:
			st = fgs(th.faint).Render("↳ via *")
			if active > 0 {
				st += "  " + fgs(th.ok).Bold(true).Render(fmt.Sprintf("⬇ %d active", active))
			}
		case !l.Enabled:
			st = fgs(th.faint).Render("off")
		case active > 0:
			// Pulse the arrow so activity is visible at a glance.
			c := th.ok
			if d.frame/3%2 == 0 {
				c = lerp(th.ok, th.fg, 0.4)
			}
			st = fgs(c).Bold(true).Render(fmt.Sprintf("⬇ %d active", active))
		case l.Conns > 0:
			st = fgs(th.muted).Render(fmt.Sprintf("● %d conn", l.Conns))
		default:
			st = fgs(lerp(th.ok, th.bg, 0.35)).Render("● ready")
		}
		rows = append(rows, cur+box+" "+addr+"  "+label+" "+st)
	}
	return th.panel("Listeners", rows, w, d.focus == focusListeners)
}

// throughputPanel shows the rolling sparkline and the running counters.
func (d *dashboard) throughputPanel(w, sparkRows int) string {
	th := d.th
	s := d.snap
	inner := w - 4
	rows := sparkline(s.History, inner, sparkRows, th)

	avg := 0.0
	for _, v := range s.History {
		avg += v
	}
	avg /= float64(len(s.History))

	b := fgs(th.fg).Bold(true)
	m := fgs(th.muted)
	rows = append(rows,
		m.Render("now ")+fgs(th.accent).Bold(true).Render(padRight(humanize.Rate(s.Rate), 12))+
			m.Render("peak ")+b.Render(padRight(humanize.Rate(s.PeakRate), 12))+
			m.Render("avg 60s ")+b.Render(humanize.Rate(avg)),
		m.Render("served ")+b.Render(humanize.Bytes(s.BytesServed))+
			m.Render(" · requests ")+b.Render(strconv.FormatInt(s.Requests, 10))+
			m.Render(" · downloads ")+b.Render(strconv.FormatInt(s.Downloads, 10))+
			m.Render(" · conns ")+b.Render(fmt.Sprintf("%d", s.OpenConns))+m.Render(fmt.Sprintf(" (peak %d)", s.PeakConns)),
	)
	hot := m.Render("hottest ") + fgs(th.faint).Render("—")
	if s.Hottest != "" {
		hot = m.Render("hottest ") + b.Render(s.Hottest) + m.Render(fmt.Sprintf("  ×%d", s.HottestHits))
	}
	rows = append(rows, hot)
	return th.panel("Throughput · last 60s", rows, w, false)
}

// sparkline renders history as a multi-row bar chart using eighth blocks,
// stretched to fill the width (each column shows the sample under it); when
// the panel is narrower than the history, only the latest seconds are shown.
func sparkline(hist []float64, width, rows int, th theme) []string {
	bars := []rune(" ▁▂▃▄▅▆▇█")
	vals := hist
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	peak := 0.0
	for _, v := range vals {
		peak = max(peak, v)
	}
	colors := lipgloss.Blend1D(max(2, len(vals)), th.accent2, th.accent)
	out := make([]string, rows)
	// Build each output row from the top; a value spans rows*8 levels.
	for r := range rows {
		var sb strings.Builder
		level := rows - 1 - r
		for c := range width {
			i := c * len(vals) / width
			v := vals[i]
			n := 0
			if peak > 0 {
				n = int(v / peak * float64(rows*8))
				// Any traffic at all shows at least a sliver.
				if v > 0 && n == 0 {
					n = 1
				}
			}
			fill := min(8, max(0, n-level*8))
			cell := string(bars[fill])
			col := colors[i]
			// Idle baseline: a faint rule on the bottom row keeps the axis visible.
			if fill == 0 && level == 0 {
				cell, col = "·", th.line
			}
			sb.WriteString(fgs(col).Render(cell))
		}
		out[r] = sb.String()
	}
	return out
}

// qrPanel shows a scannable code for the share URL, padded to height h.
func (d *dashboard) qrPanel(w, h int) string {
	th := d.th
	rows := append([]string{}, d.qr.lines...)
	rows = append(rows, "")
	u := d.qr.text
	note := "selected listener"
	if strings.Contains(u, "127.0.0.1") {
		note = "loopback: this machine only"
	}
	rows = append(rows, fgs(th.accent).Render(strings.TrimPrefix(u, "http://")))
	for len(rows) < h-3 {
		rows = append(rows, "")
	}
	rows = append(rows, fgs(th.faint).Render(note))
	return th.panel("Scan to connect", rows, w, false)
}

// transfersPanel lists in-flight and recently finished downloads.
func (d *dashboard) transfersPanel(w, maxRows int) string {
	th := d.th
	inner := w - 4
	ts := d.snap.Transfers
	title := fmt.Sprintf("Transfers · %d active", d.activeCount())

	if len(ts) == 0 {
		return th.panel(title, []string{fgs(th.faint).Render("No transfers yet. Open one of the URLs above on another device.")}, w, d.focus == focusTransfers)
	}

	// Keep the selection in view by scrolling the window.
	start := 0
	if sel := d.selectedIndex(); sel >= maxRows {
		start = sel - maxRows + 1
	}
	end := min(len(ts), start+maxRows)

	showBytes := inner >= 90
	showETA := inner >= 104
	nameW := inner - 2 - 16 - 18 - 11
	if showBytes {
		nameW -= 21
	}
	if showETA {
		nameW -= 8
	}
	nameW = max(8, nameW)

	var rows []string
	// One line per transfer; finished ones are dimmed.
	for i := start; i < end; i++ {
		t := ts[i]
		finished := t.State != stats.StateActive
		base := fgs(th.fg)
		if finished {
			base = fgs(th.faint)
		}
		cur := "  "
		if t.ID == d.tsel && d.focus == focusTransfers {
			cur = fgs(th.accent).Render("▸ ")
		}
		name := t.Name
		if t.Kind == stats.KindZip {
			name += " (zip)"
		}
		row := cur + base.Render(padRight(t.Client, 16)) + base.Render(fit(name, nameW)) + " " +
			d.progressCell(t, 17) + " " + base.Render(padRight(rateCell(t), 10))
		if showBytes {
			row += " " + base.Render(padRight(bytesCell(t), 20))
		}
		if showETA {
			row += " " + fgs(th.muted).Render(padRight(humanize.ETA(t.Sent, t.Total, t.Rate), 7))
		}
		rows = append(rows, row)
	}
	return th.panel(title, rows, w, d.focus == focusTransfers)
}

// progressCell renders a gradient bar + percentage, a live spinner for
// unknown sizes, or the final state.
func (d *dashboard) progressCell(t stats.View, w int) string {
	th := d.th
	switch t.State {
	case stats.StateDone:
		return fgs(lerp(th.ok, th.bg, 0.3)).Render(padRight("✓ done", w))
	case stats.StateAborted:
		return fgs(lerp(th.err, th.bg, 0.3)).Render(padRight("✗ aborted", w))
	}
	p := t.Progress()
	// Unknown total (ZIP stream): show that it's alive rather than a bar.
	if p < 0 {
		sp := string(spinner[d.frame%len(spinner)])
		return fgs(th.accent).Render(sp) + fgs(th.muted).Render(padRight(" streaming", w-1))
	}
	barW := w - 5
	filled := int(p * float64(barW))
	colors := lipgloss.Blend1D(max(2, barW), th.accent2, th.accent)
	var sb strings.Builder
	for i := range barW {
		if i < filled {
			sb.WriteString(fgs(colors[i]).Render("━"))
		} else {
			sb.WriteString(fgs(th.line).Render("━"))
		}
	}
	return sb.String() + fgs(th.fg).Render(fmt.Sprintf(" %3.0f%%", p*100))
}

// rateCell is the transfer's current speed ("" once finished).
func rateCell(t stats.View) string {
	if t.State != stats.StateActive {
		return ""
	}
	return humanize.Rate(t.Rate)
}

// bytesCell is "sent / total" or just "sent" if the total is unknown.
func bytesCell(t stats.View) string {
	if t.Total < 0 {
		return humanize.Bytes(t.Sent)
	}
	return humanize.Bytes(t.Sent) + " / " + humanize.Bytes(t.Total)
}

// logPanel shows the most recent requests, newest first.
func (d *dashboard) logPanel(w, n int) string {
	th := d.th
	var rows []string
	inner := w - 4
	pathW := max(10, inner-10-16-6-5-10-8)
	// Newest first, as many as fit.
	for i, e := range d.snap.Log {
		if i >= n {
			break
		}
		sc := th.ok
		switch {
		case e.Status >= 500:
			sc = th.err
		case e.Status >= 400:
			sc = th.warn
		case e.Status >= 300:
			sc = th.muted
		}
		p, _ := url.PathUnescape(e.Path)
		rows = append(rows,
			fgs(th.faint).Render(e.Time.Format("15:04:05")+"  ")+
				fgs(th.muted).Render(padRight(e.Client, 16))+
				fgs(th.fg).Render(padRight(e.Method, 6))+
				fgs(sc).Render(padRight(strconv.Itoa(e.Status), 5))+
				fgs(th.fg).Render(fit(p, pathW))+" "+
				fgs(th.muted).Render(padRight(humanize.Bytes(e.Bytes), 10))+
				fgs(th.faint).Render(humanize.Duration(e.Duration)))
	}
	if len(rows) == 0 {
		rows = append(rows, fgs(th.faint).Render("Waiting for the first request…"))
	}
	return th.panel("Recent requests", rows, w, false)
}

// footer shows key help on the left and any transient message on the right.
func (d *dashboard) footer(w int) string {
	th := d.th
	msg := ""
	if d.flash != "" && time.Now().Before(d.flashUntil) {
		c := th.accent
		if d.flashErr {
			c = th.warn
		}
		msg = fgs(c).Render(d.flash)
	}
	// Full key list: a multi-line block, with any message on its own line.
	if d.help.ShowAll {
		lines := strings.Split(d.help.FullHelpView(d.keys.FullHelp()), "\n")
		for i, l := range lines {
			lines[i] = " " + fit(l, w-1)
		}
		if msg != "" {
			lines = append(lines, " "+fit(msg, w-1))
		}
		return strings.Join(lines, "\n")
	}
	helpView := d.help.ShortHelpView(d.keys.ShortHelp())
	if msg == "" {
		return " " + fit(helpView, w-1)
	}
	// Prefer the message when space is tight; it's the newer information.
	room := w - lipgloss.Width(msg) - 3
	if room < 20 {
		return " " + fit(msg, w-1)
	}
	return " " + fit(helpView, room) + "  " + msg
}

// tildePath abbreviates the home directory as "~".
func tildePath(p string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}
