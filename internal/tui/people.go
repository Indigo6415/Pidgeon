package tui

import (
	"crypto/rand"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Indigo6415/pidgeon/internal/brand"
	"github.com/Indigo6415/pidgeon/internal/devices"
	"github.com/Indigo6415/pidgeon/internal/humanize"
	"github.com/Indigo6415/pidgeon/internal/server"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// This file holds the dashboard's device and share-link features: the two
// panels, the --ask approval prompts, and the menus that create share links
// and send files to a device (with a small file picker).

/* ------------------------------------------------------------------ */
/* approval prompts                                                   */
/* ------------------------------------------------------------------ */

// syncPrompt keeps the approval popup in step with the registry: it closes
// a prompt that was answered or withdrawn elsewhere, and opens the oldest
// open prompt the operator hasn't put off whenever no other menu is up.
func (d *dashboard) syncPrompt() {
	if d.o.Devices == nil {
		return
	}
	open := map[uint64]bool{}
	for _, p := range d.prompts {
		open[p.ID] = true
	}
	// Forget put-off prompts that no longer exist.
	for id := range d.deferred {
		if !open[id] {
			delete(d.deferred, id)
		}
	}
	if d.menu != nil && d.menu.prompt != 0 && !open[d.menu.prompt] {
		d.menu = nil
	}
	if d.menu != nil {
		return
	}
	// The oldest prompt that hasn't been put off gets the popup.
	for _, p := range d.prompts {
		if !d.deferred[p.ID] {
			d.menu = d.promptMenu(p)
			return
		}
	}
}

// promptMenu asks the operator to decide one prompt.
func (d *dashboard) promptMenu(p devices.Prompt) *menu {
	who := p.IP
	if p.Agent != "" {
		who = p.Agent + " · " + p.IP
	}
	reg := d.o.Devices
	later := menuItem{label: "Decide later", hint: "it keeps waiting", run: func() tea.Cmd { d.deferred[p.ID] = true; return nil }}
	var m *menu
	if p.Kind == devices.PromptUpload {
		size := "unknown size"
		if p.Size >= 0 {
			size = humanize.Bytes(p.Size)
		}
		m = newMenu("Upload request",
			menuItem{label: "Accept", key: "y", hint: "save it in the inbox", run: func() tea.Cmd {
				reg.Decide(p.ID, true, false)
				d.say("accepting "+p.Name+" from "+who, false)
				return nil
			}},
			menuItem{label: "Decline", key: "n", danger: true, run: func() tea.Cmd {
				reg.Decide(p.ID, false, false)
				d.say("declined "+p.Name, false)
				return nil
			}},
			later,
		)
		m.note = []string{who + " wants to send you", p.Name + " · " + size}
	} else {
		m = newMenu("New device",
			menuItem{label: "Allow", key: "y", hint: "for this session", run: func() tea.Cmd {
				reg.Decide(p.ID, true, false)
				d.say("allowed "+who, false)
				return nil
			}},
			menuItem{label: "Always allow", key: "a", hint: "remember this address", run: func() tea.Cmd {
				reg.Decide(p.ID, true, true)
				d.say("allowed "+who+" (remembered)", false)
				return nil
			}},
			menuItem{label: "Block", key: "n", danger: true, run: func() tea.Cmd {
				reg.Decide(p.ID, false, false)
				d.say("blocked "+who, false)
				return nil
			}},
			later,
		)
		m.note = []string{who + " wants to connect."}
	}
	m.prompt = p.ID
	return m
}

/* ------------------------------------------------------------------ */
/* devices                                                            */
/* ------------------------------------------------------------------ */

// deviceMenu lists the actions for one device.
func (d *dashboard) deviceMenu(dev devices.Device) *menu {
	reg := d.o.Devices
	ip := dev.IP
	items := []menuItem{}
	send := menuItem{label: "Send a file…", hint: "pops up in its browser", run: func() tea.Cmd { return d.browse("/", pickSend, ip) }}
	// Sending needs a page open on the device to receive it.
	if d.online[ip] == 0 {
		send.disabled, send.hint = true, "no page open on it"
	}
	items = append(items, send,
		menuItem{label: "Send text…", hint: "Pigeon Post", run: func() tea.Cmd { return d.composePost(ip) }})
	if dev.Status != devices.Allowed {
		items = append(items, menuItem{label: "Allow", run: func() tea.Cmd { reg.Allow(ip, false); d.say("allowed "+dev.Label(), false); return nil }})
	}
	if !dev.Trusted && !dev.Local && dev.Status != devices.Blocked {
		items = append(items, menuItem{label: "Always allow", hint: "remember this address", run: func() tea.Cmd {
			reg.Allow(ip, true)
			d.say("will always allow "+dev.Label(), false)
			return nil
		}})
	}
	block := menuItem{label: "Block", hint: "refuse it and stop its transfers", danger: true, run: func() tea.Cmd {
		reg.Block(ip)
		n := d.o.Store.CancelClient(ip)
		d.say(fmt.Sprintf("blocked %s (%d transfer(s) stopped)", dev.Label(), n), false)
		return nil
	}}
	forget := menuItem{label: "Disconnect", hint: "stop its transfers and forget it", danger: true, run: func() tea.Cmd {
		n := d.o.Store.CancelClient(ip)
		reg.Forget(ip)
		d.say(fmt.Sprintf("disconnected %s (%d transfer(s) stopped)", dev.Label(), n), false)
		return nil
	}}
	if reg.Ask() {
		forget.hint = "stop its transfers; it must be allowed again"
	}
	// This machine can't lock itself out.
	if dev.Local {
		block.disabled, block.hint = true, "this machine"
	}
	if dev.Status == devices.Blocked {
		block.disabled, block.hint = true, "already blocked"
	}
	items = append(items, block, forget, menuItem{label: "Back", run: func() tea.Cmd { return nil }})
	m := newMenu(dev.Label(), items...)
	m.note = []string{d.deviceSummary(dev)}
	return m
}

// deviceSummary is a one-line description of a device for its menu.
func (d *dashboard) deviceSummary(dev devices.Device) string {
	parts := []string{dev.Status.String()}
	if dev.Trusted {
		parts = append(parts, "remembered")
	}
	parts = append(parts, "first seen "+humanize.Duration(time.Since(dev.FirstSeen))+" ago",
		fmt.Sprintf("%d requests", dev.Requests), "↑ "+humanize.Bytes(dev.Sent))
	if dev.Received > 0 {
		parts = append(parts, "↓ "+humanize.Bytes(dev.Received))
	}
	return strings.Join(parts, " · ")
}

// devicesPanel lists every device that has talked to the server.
func (d *dashboard) devicesPanel(w, maxRows int) string {
	th := d.th
	focused := d.focus == focusDevices
	title := fmt.Sprintf("Devices · %d", len(d.devs))
	if d.o.Devices != nil && d.o.Devices.Ask() {
		title += " · ask mode"
	}
	if len(d.devs) == 0 {
		return th.panel(title, padRows([]string{fgs(th.faint).Render("No devices yet. New ones wait here for your OK.")}, maxRows), w, focused)
	}
	active := map[string]int{}
	for _, t := range d.snap.Transfers {
		if t.State == stats.StateActive {
			active[t.Client]++
		}
	}
	// Columns: cursor+dot (4), address (16), nickname, status (16), traffic.
	inner := w - 4
	agentW := max(8, min(20, inner-4-16-2-16-1-10))
	start := scrollStart(d.devIndex(), len(d.devs), maxRows)
	var rows []string
	// One line per device: status, address, nickname, activity.
	for _, dev := range d.devs[start:min(len(d.devs), start+maxRows)] {
		cur := "  "
		if dev.IP == d.dsel && focused {
			cur = fgs(th.accent).Render("▸ ")
		}
		var dot string
		switch dev.Status {
		case devices.Pending:
			// Pulse so a waiting device catches the eye.
			c := th.warn
			if d.frame/4%2 == 0 {
				c = lerp(th.warn, th.bg, 0.5)
			}
			dot = fgs(c).Bold(true).Render("◌")
		case devices.Blocked:
			dot = fgs(th.err).Render("✕")
		default:
			dot = fgs(th.ok).Render("●")
			if d.online[dev.IP] == 0 {
				dot = fgs(th.faint).Render("●")
			}
		}
		agent := dev.Agent
		if dev.Local {
			agent = "this machine"
		}
		if dev.Trusted {
			agent += " ★"
		}
		var st string
		switch {
		case dev.Status == devices.Pending:
			st = fgs(th.warn).Render("waiting for you")
		case dev.Status == devices.Blocked:
			st = fgs(th.err).Render("blocked")
		case active[dev.IP] > 0:
			st = fgs(th.ok).Bold(true).Render(fmt.Sprintf("⬇ %d active", active[dev.IP]))
		case d.online[dev.IP] > 0:
			st = fgs(th.muted).Render(fmt.Sprintf("%d tab(s) open", d.online[dev.IP]))
		default:
			st = fgs(th.faint).Render("seen " + humanize.Duration(time.Since(dev.LastSeen)) + " ago")
		}
		traffic := "↑ " + humanize.Bytes(dev.Sent)
		if dev.Received > 0 {
			traffic += "  ↓ " + humanize.Bytes(dev.Received)
		}
		rows = append(rows, cur+dot+" "+fgs(th.fg).Render(padRight(dev.IP, 16))+fgs(th.muted).Render(fit(agent, agentW))+"  "+
			fit(st, 16)+" "+fgs(th.faint).Render(traffic))
	}
	return th.panel(title, padRows(rows, maxRows), w, focused)
}

/* ------------------------------------------------------------------ */
/* share links                                                        */
/* ------------------------------------------------------------------ */

// sharesPanel lists the share links, newest first.
func (d *dashboard) sharesPanel(w, maxRows int) string {
	th := d.th
	focused := d.focus == focusShares
	live := 0
	for _, sh := range d.shares {
		if sh.State == "active" {
			live++
		}
	}
	title := fmt.Sprintf("Share links · %d active", live)
	inner := w - 4
	nameW := max(8, inner-2-12-14)
	start := scrollStart(d.shareIndex(), len(d.shares), maxRows)
	var rows []string
	// One line per link: target, uses, time left or why it stopped.
	for _, sh := range d.shares[start:min(len(d.shares), start+maxRows)] {
		cur := "  "
		if sh.ID == d.ssel && focused {
			cur = fgs(th.accent).Render("▸ ")
		}
		base := fgs(th.fg)
		if sh.State != "active" {
			base = fgs(th.faint)
		}
		name := sh.Path
		if sh.Dir && name != "/" {
			name += "/"
		}
		uses := fmt.Sprintf("%d used", sh.Uses)
		if sh.MaxUses > 0 {
			uses = fmt.Sprintf("%d/%d used", sh.Uses, sh.MaxUses)
		}
		// A file request collects files instead of handing them out.
		if sh.Request {
			name = "⇣ " + name
			uses = fmt.Sprintf("%d in", sh.Uses)
			if sh.MaxUses > 0 {
				uses = fmt.Sprintf("%d/%d in", sh.Uses, sh.MaxUses)
			}
		}
		if sh.PIN != "" {
			name += " · PIN"
		}
		var st string
		switch {
		case sh.State != "active":
			st = fgs(th.err).Render(sh.State)
		case sh.Expires.IsZero():
			st = fgs(th.muted).Render("no expiry")
		default:
			st = fgs(th.muted).Render(humanize.Duration(time.Until(sh.Expires)) + " left")
		}
		rows = append(rows, cur+base.Render(fit(name, nameW))+" "+base.Render(padRight(uses, 11))+" "+st)
	}
	return th.panel(title, padRows(rows, maxRows), w, focused)
}

// shareMenu lists the actions for one share link.
func (d *dashboard) shareMenu(sh server.Share) *menu {
	live := sh.State == "active"
	cp := menuItem{label: "Copy link", key: "c", run: func() tea.Cmd { d.say("copied "+sh.URL, false); return tea.SetClipboard(sh.URL) }}
	op := menuItem{label: "Open in browser", key: "o", run: func() tea.Cmd { return openBrowser(sh.URL) }}
	revoke := menuItem{label: "Revoke", hint: "the link stops working now", danger: true, run: func() tea.Cmd {
		d.o.Server.RevokeShare(sh.ID)
		d.say("revoked the link to "+sh.Path, false)
		return nil
	}}
	remove := menuItem{label: "Remove from list", run: func() tea.Cmd { d.o.Server.RemoveShare(sh.ID); return nil }}
	// A dead link can only be removed.
	if !live {
		cp.disabled, op.disabled, revoke.disabled = true, true, true
		revoke.hint = sh.State
	}
	title := "Share link · " + sh.Path
	if sh.Request {
		title = "File request · " + sh.Name
	}
	m := newMenu(title, cp, op, revoke, remove, menuItem{label: "Back", run: func() tea.Cmd { return nil }})
	m.note = []string{sh.URL}
	if sh.PIN != "" {
		m.note = append(m.note, "PIN "+sh.PIN)
	}
	if sh.Request {
		m.note = append(m.note, fmt.Sprintf("Files land in %s (%d so far).", sh.Path, sh.Uses))
	}
	return m
}

// startShare begins creating a share link: pick a file or folder first.
func (d *dashboard) startShare() tea.Cmd {
	if d.o.Server == nil {
		return nil
	}
	return d.browse("/", pickShare, "")
}

// startRequest begins a file request: a link others upload files with,
// into a folder of the inbox. First the folder's name.
func (d *dashboard) startRequest() tea.Cmd {
	if d.o.Server == nil || d.o.Server.Inbox() == "" {
		d.say("file requests need an inbox: start with --inbox <dir>", true)
		return nil
	}
	def := "Request " + time.Now().Format("2006-01-02 1504")
	note := []string{"Name the inbox folder the files go into.", "Leave it empty for “" + def + "”."}
	return d.openInput("Request files", note, def, 120, func(name string) tea.Cmd {
		return d.shareExpiry(server.ShareOptions{Request: true, Folder: strings.TrimSpace(name)})
	})
}

// shareSubject describes the link being made, for the menus' notes.
func shareSubject(o server.ShareOptions) string {
	if o.Request {
		name := o.Folder
		if name == "" {
			name = "a new folder"
		}
		return "Requesting files into " + name
	}
	return "Sharing " + o.Path
}

// shareExpiry asks how long the new link should last.
func (d *dashboard) shareExpiry(o server.ShareOptions) tea.Cmd {
	opt := func(label string, ttl time.Duration) menuItem {
		return menuItem{label: label, run: func() tea.Cmd { o.TTL = ttl; return d.shareUses(o) }}
	}
	m := newMenu("Link expires",
		opt("in 15 minutes", 15*time.Minute), opt("in 1 hour", time.Hour), opt("in 1 day", 24*time.Hour),
		opt("in 7 days", 7*24*time.Hour), opt("never (while "+brand.Name+" runs)", 0),
	)
	m.cur = 1 // an hour is a sensible default
	m.note = []string{shareSubject(o)}
	d.menu = m
	return nil
}

// shareUses asks how many downloads (or, for a request, files) the new
// link allows.
func (d *dashboard) shareUses(o server.ShareOptions) tea.Cmd {
	opt := func(label string, n int) menuItem {
		return menuItem{label: label, run: func() tea.Cmd { o.MaxUses = n; return d.sharePIN(o) }}
	}
	var m *menu
	if o.Request {
		m = newMenu("Files accepted", opt("1 file", 1), opt("10 files", 10), opt("100 files", 100), opt("unlimited", 0))
		m.cur = 3
		m.note = []string{shareSubject(o), "Copies of files already in the inbox don't count."}
	} else {
		m = newMenu("Downloads allowed", opt("1 download", 1), opt("3 downloads", 3), opt("10 downloads", 10), opt("unlimited", 0))
		m.note = []string{shareSubject(o), "Resuming a started download doesn't count."}
	}
	d.menu = m
	return nil
}

// sharePIN asks whether visitors must type a PIN first, then makes the link.
func (d *dashboard) sharePIN(o server.ShareOptions) tea.Cmd {
	with := func(pin string) tea.Cmd { o.PIN = pin; return d.createShare(o) }
	m := newMenu("PIN",
		menuItem{label: "No PIN", hint: "the link alone is enough", run: func() tea.Cmd { return with("") }},
		menuItem{label: "Random 4-digit PIN", run: func() tea.Cmd { return with(randomPIN(4)) }},
		menuItem{label: "Random 6-digit PIN", run: func() tea.Cmd { return with(randomPIN(6)) }},
		menuItem{label: "Choose a PIN…", run: func() tea.Cmd { return d.choosePIN(o) }},
	)
	m.note = []string{shareSubject(o), "Tell the PIN separately (say it, text it): whoever", "intercepts the link alone still can't open it."}
	d.menu = m
	return nil
}

// choosePIN asks for a PIN of the operator's choice.
func (d *dashboard) choosePIN(o server.ShareOptions) tea.Cmd {
	return d.openInput("Choose a PIN", []string{"4 to 12 digits."}, "e.g. 2468", 12, func(pin string) tea.Cmd {
		pin = strings.TrimSpace(pin)
		if !validPIN(pin) {
			d.say("a PIN is 4 to 12 digits", true)
			return d.choosePIN(o)
		}
		o.PIN = pin
		return d.createShare(o)
	})
}

// validPIN reports whether p is 4 to 12 digits.
func validPIN(p string) bool {
	if len(p) < 4 || len(p) > 12 {
		return false
	}
	for _, c := range p {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// randomPIN makes an n-digit PIN.
func randomPIN(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	// Map each byte to a digit; the slight bias doesn't matter for a PIN
	// that locks after ten wrong guesses.
	for i := range b {
		b[i] = '0' + b[i]%10
	}
	return string(b)
}

// createShare makes the link, selects it (so the QR code shows it) and
// copies it to the clipboard.
func (d *dashboard) createShare(o server.ShareOptions) tea.Cmd {
	sh, err := d.o.Server.NewShare(o)
	if err != nil {
		d.say("couldn't make the link: "+err.Error(), true)
		return nil
	}
	d.ssel = sh.ID
	d.focus = focusShares
	d.refresh()
	what := "share link"
	if sh.Request {
		what = "file request"
	}
	msg := what + " copied — scan the QR code or paste it: " + sh.URL
	if sh.PIN != "" {
		msg = what + " copied (PIN " + sh.PIN + ") — " + sh.URL
	}
	d.say(msg, false)
	return tea.SetClipboard(sh.URL)
}

/* ------------------------------------------------------------------ */
/* sending files                                                      */
/* ------------------------------------------------------------------ */

// startSend picks a device with the page open, then a file to send it.
func (d *dashboard) startSend() tea.Cmd {
	var items []menuItem
	// Only devices with a tab open can receive.
	for _, dev := range d.devs {
		if d.online[dev.IP] == 0 || dev.Status != devices.Allowed {
			continue
		}
		ip := dev.IP
		items = append(items, menuItem{label: dev.Label(), hint: fmt.Sprintf("%d tab(s)", d.online[ip]), run: func() tea.Cmd {
			return d.browse("/", pickSend, ip)
		}})
	}
	if len(items) == 0 {
		d.say("no device has the page open right now", true)
		return nil
	}
	if len(items) > 1 {
		items = append(items, menuItem{label: "Every device", run: func() tea.Cmd { return d.browse("/", pickSend, "") }})
	}
	d.menu = newMenu("Send to which device?", items...)
	return nil
}

// sendTo offers file p to every open tab on device ip ("" = all devices).
func (d *dashboard) sendTo(ip, p string) tea.Cmd {
	n, err := d.o.Server.Send(ip, p)
	if err != nil {
		d.say("couldn't send "+path.Base(p)+": "+err.Error(), true)
		return nil
	}
	who := "every device"
	if dev, ok := d.o.Devices.Get(ip); ok {
		who = dev.Label()
	}
	d.say(fmt.Sprintf("sent %s to %s (%d tab(s))", path.Base(p), who, n), false)
	return nil
}

/* ------------------------------------------------------------------ */
/* file picker                                                        */
/* ------------------------------------------------------------------ */

// pickMode is what the file picker is choosing for.
type pickMode int

const (
	pickShare pickMode = iota // a file or folder to share
	pickSend                  // a file to send to a device
)

// pickMax bounds the entries listed for one folder.
const pickMax = 500

// browse opens the file picker on dir ("/" or "/a/b"). Enter opens a folder
// or picks a file, ← goes up, esc gives up.
func (d *dashboard) browse(dir string, mode pickMode, ip string) tea.Cmd {
	es, err := d.o.Server.Browse(dir)
	if err != nil {
		d.say("can't open "+dir+": "+err.Error(), true)
		return nil
	}
	// Folders first, then files, each alphabetically.
	slices.SortFunc(es, func(a, b server.Entry) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	parent := path.Dir(dir)
	var items []menuItem
	if mode == pickShare {
		items = append(items, menuItem{label: "★ Share this folder", hint: dir, run: func() tea.Cmd { return d.shareExpiry(server.ShareOptions{Path: dir}) }})
	}
	if dir != "/" {
		items = append(items, menuItem{label: "../", hint: "up", run: func() tea.Cmd { return d.browse(parent, mode, ip) }})
	}
	// One item per entry; folders open, files are picked.
	for i, e := range es {
		if i >= pickMax {
			items = append(items, menuItem{label: fmt.Sprintf("… %d more not shown", len(es)-pickMax), disabled: true})
			break
		}
		p := path.Join(dir, e.Name)
		if e.Dir {
			items = append(items, menuItem{label: e.Name + "/", run: func() tea.Cmd { return d.browse(p, mode, ip) }})
			continue
		}
		it := menuItem{label: e.Name, hint: humanize.Bytes(e.Size)}
		if mode == pickShare {
			it.run = func() tea.Cmd { return d.shareExpiry(server.ShareOptions{Path: p}) }
		} else {
			it.run = func() tea.Cmd { return d.sendTo(ip, p) }
		}
		items = append(items, it)
	}
	if len(es) == 0 {
		items = append(items, menuItem{label: "(empty folder)", disabled: true})
	}
	title := "Share · " + dir
	if mode == pickSend {
		who := "every device"
		if dev, ok := d.o.Devices.Get(ip); ok {
			who = dev.Label()
		}
		title = "Send to " + who + " · " + dir
	}
	m := newMenu(title, items...)
	m.back = func() tea.Cmd {
		// At the top, ← closes the picker like esc.
		if dir == "/" {
			return nil
		}
		return d.browse(parent, mode, ip)
	}
	d.menu = m
	return nil
}

/* ------------------------------------------------------------------ */
/* helpers                                                            */
/* ------------------------------------------------------------------ */

// qrTarget is what the QR code, copy and open point at: the selected share
// link while the share panel has focus, otherwise the listener URL.
func (d *dashboard) qrTarget() (u, note string) {
	if d.focus == focusShares {
		if i := d.shareIndex(); i >= 0 && d.shares[i].State == "active" {
			sh := d.shares[i]
			note = "share link · " + path.Base(sh.Path)
			if sh.Request {
				note = "file request · " + sh.Name
			}
			if sh.MaxUses > 0 {
				note += fmt.Sprintf(" · %d left", sh.MaxUses-sh.Uses)
			}
			if sh.PIN != "" {
				note += " · PIN " + sh.PIN
			}
			return sh.URL, note
		}
	}
	u = d.shareURL()
	note = "selected listener"
	if strings.Contains(u, "127.0.0.1") {
		note = "loopback: this machine only"
	}
	return u, note
}

// scrollStart picks the first visible row so that sel stays on screen.
func scrollStart(sel, n, rows int) int {
	if sel < rows {
		return 0
	}
	return min(sel-rows+1, max(0, n-rows))
}

// padRows pads a panel body to exactly n lines (so side-by-side panels
// line up).
func padRows(rows []string, n int) []string {
	for len(rows) < n {
		rows = append(rows, "")
	}
	return rows
}
