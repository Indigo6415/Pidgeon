package tui

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Indigo6415/pidgeon/internal/devices"
	"github.com/Indigo6415/pidgeon/internal/humanize"
	"github.com/Indigo6415/pidgeon/internal/server"
)

// This file holds Pigeon Post (text between the host and the devices) and
// the one-line text input it uses, which the share flows reuse for folder
// names and PINs.

/* ------------------------------------------------------------------ */
/* text input                                                         */
/* ------------------------------------------------------------------ */

// input is a modal one-line text field. While it is open it gets every key
// (and pasted text); enter submits, esc gives up.
type input struct {
	title       string
	note        []string
	placeholder string
	limit       int    // most runes accepted
	text        []rune // what has been typed
	pos         int    // cursor position in text
	submit      func(string) tea.Cmd
}

// openInput shows a text field. submit runs with the text after the field
// closes, so it may open a menu or another field.
func (d *dashboard) openInput(title string, note []string, placeholder string, limit int, submit func(string) tea.Cmd) tea.Cmd {
	d.menu = nil
	d.input = &input{title: title, note: note, placeholder: placeholder, limit: limit, submit: submit}
	return nil
}

// inputKey drives the open text field: a small line editor with the usual
// terminal keys (arrows, home/end, ctrl+a/e/u/k/w, backspace, delete).
func (d *dashboard) inputKey(msg tea.KeyPressMsg) tea.Cmd {
	in := d.input
	switch msg.String() {
	case "esc":
		d.input = nil
	case "enter":
		d.input = nil
		return in.submit(string(in.text))
	case "left", "ctrl+b":
		in.pos = max(0, in.pos-1)
	case "right", "ctrl+f":
		in.pos = min(len(in.text), in.pos+1)
	case "home", "ctrl+a":
		in.pos = 0
	case "end", "ctrl+e":
		in.pos = len(in.text)
	case "backspace", "ctrl+h":
		if in.pos > 0 {
			in.text = append(in.text[:in.pos-1], in.text[in.pos:]...)
			in.pos--
		}
	case "delete", "ctrl+d":
		if in.pos < len(in.text) {
			in.text = append(in.text[:in.pos], in.text[in.pos+1:]...)
		}
	case "ctrl+u":
		in.text, in.pos = in.text[in.pos:], 0
	case "ctrl+k":
		in.text = in.text[:in.pos]
	case "ctrl+w", "alt+backspace":
		// Delete the word before the cursor (and the spaces after it).
		i := in.pos
		for i > 0 && in.text[i-1] == ' ' {
			i--
		}
		for i > 0 && in.text[i-1] != ' ' {
			i--
		}
		in.text, in.pos = append(in.text[:i], in.text[in.pos:]...), i
	default:
		// Printable keys carry their text; everything else is ignored.
		if msg.Text != "" && msg.Mod&(tea.ModCtrl|tea.ModAlt) == 0 {
			in.insert(msg.Text)
		}
	}
	return nil
}

// paste inserts pasted text into the open field (line breaks become spaces:
// the field is one line, the message can still hold them when typed in a
// browser).
func (d *dashboard) paste(text string) {
	if d.input != nil {
		d.input.insert(strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(text))
	}
}

// insert puts text at the cursor, up to the limit.
func (in *input) insert(text string) {
	r := []rune(text)
	if in.limit > 0 {
		r = r[:min(len(r), max(0, in.limit-len(in.text)))]
	}
	in.text = append(in.text[:in.pos], append(r, in.text[in.pos:]...)...)
	in.pos += len(r)
}

// view renders the text field as a box like the menus.
func (in *input) view(th theme) string {
	const width = 64
	var rows []string
	// Notes wrap rather than being cut off.
	for _, n := range wrapAll(in.note, width-4) {
		rows = append(rows, fgs(th.muted).Render(n))
	}
	if len(in.note) > 0 {
		rows = append(rows, "")
	}
	rows = append(rows, fgs(th.accent).Render("› ")+in.line(th, width-8), "", fgs(th.faint).Render("enter ok · esc cancel"))
	return th.panel(in.title, rows, width, true)
}

// line draws the text with a block cursor, scrolled so the cursor shows.
func (in *input) line(th theme, w int) string {
	cursor := lipgloss.NewStyle().Reverse(true)
	if len(in.text) == 0 {
		return cursor.Render(" ") + fgs(th.faint).Render(fit(in.placeholder, w-1))
	}
	start := max(0, in.pos-w+1)
	end := min(len(in.text), start+w-1)
	before, at, after := string(in.text[start:in.pos]), " ", ""
	if in.pos < len(in.text) {
		at = string(in.text[in.pos])
		after = string(in.text[in.pos+1 : max(in.pos+1, end)])
	}
	return fgs(th.fg).Render(before) + cursor.Render(at) + fgs(th.fg).Render(after)
}

/* ------------------------------------------------------------------ */
/* Pigeon Post                                                        */
/* ------------------------------------------------------------------ */

// syncPosts queues the messages devices sent since the last refresh, and
// pops the oldest one up when nothing else is on screen.
func (d *dashboard) syncPosts() {
	if d.o.Server == nil {
		return
	}
	d.posts = d.o.Server.Messages()
	// New messages from devices wait their turn to be shown.
	for _, m := range d.posts {
		if m.ID > d.postSeen {
			d.postSeen = m.ID
			if m.From != "" {
				d.postQueue = append(d.postQueue, m)
			}
		}
	}
	if d.menu != nil || d.input != nil || len(d.postQueue) == 0 {
		return
	}
	m := d.postQueue[0]
	d.postQueue = d.postQueue[1:]
	d.menu = d.postMenu(m)
}

// urlPattern finds the first web link in a message.
var urlPattern = regexp.MustCompile(`https?://[^\s<>"']+`)

// postMenu shows one message with what can be done with it.
func (d *dashboard) postMenu(m server.Message) *menu {
	who := d.who(m.From)
	items := []menuItem{
		{label: "Copy to clipboard", key: "c", run: func() tea.Cmd {
			d.say(fmt.Sprintf("copied the message from %s (%d characters)", who, len([]rune(m.Text))), false)
			return tea.SetClipboard(m.Text)
		}},
	}
	if u := urlPattern.FindString(m.Text); u != "" {
		items = append(items, menuItem{label: "Open link", key: "o", hint: u, run: func() tea.Cmd { return openBrowser(u) }})
	}
	if m.From != "" {
		ip := m.From
		items = append(items, menuItem{label: "Reply…", key: "r", run: func() tea.Cmd { return d.composePost(ip) }})
	}
	items = append(items, menuItem{label: "Close", run: func() tea.Cmd { return nil }})
	title := "Message from " + who
	if m.From == "" {
		title = "You sent"
	}
	mn := newMenu(title, items...)
	mn.note = append(wrapText(m.Text, menuMaxWidth-6, 10), fgs(d.th.faint).Render(humanize.Duration(time.Since(m.Time))+" ago"))
	return mn
}

// who names a device by its nickname and address.
func (d *dashboard) who(ip string) string {
	if ip == "" {
		return "every device"
	}
	if d.o.Devices != nil {
		if dev, ok := d.o.Devices.Get(ip); ok {
			return dev.Label()
		}
	}
	return ip
}

// wrapText breaks text into lines of at most w columns, keeping at most n.
func wrapText(text string, w, n int) []string {
	var out []string
	// Respect the message's own line breaks, then wrap each line.
	for line := range strings.SplitSeq(text, "\n") {
		r := []rune(strings.TrimRight(line, "\r"))
		for len(r) > w {
			cut := w
			// Prefer breaking at a space.
			if i := lastSpace(r[:w]); i > w/2 {
				cut = i + 1
			}
			out = append(out, string(r[:cut]))
			r = r[cut:]
		}
		out = append(out, string(r))
		if len(out) >= n {
			return append(out[:n], "…")
		}
	}
	return out
}

// lastSpace finds the last space in r, or -1.
func lastSpace(r []rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == ' ' {
			return i
		}
	}
	return -1
}

// composePost asks for text to send to device ip ("" = every device).
func (d *dashboard) composePost(ip string) tea.Cmd {
	note := []string{"To " + d.who(ip) + ". It pops up in their browser; paste works too."}
	if ip != "" && d.online[ip] == 0 {
		note = []string{"To " + d.who(ip) + ". It has no page open: it will see the message next time."}
	}
	return d.openInput("Pigeon Post", note, "Type or paste text, a link, a code…", 16<<10, func(text string) tea.Cmd {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		_, n, err := d.o.Server.Post(ip, text)
		switch {
		case err != nil:
			d.say("couldn't send: "+err.Error(), true)
		case n == 0:
			d.say("message kept for "+d.who(ip)+" (no page open right now)", false)
		default:
			d.say(fmt.Sprintf("sent to %s (%d tab(s))", d.who(ip), n), false)
		}
		return nil
	})
}

// postMainMenu is Pigeon Post in the main menu: send something, or look at
// recent messages.
func (d *dashboard) postMainMenu() tea.Cmd {
	items := []menuItem{{label: "Send text to a device…", run: d.pickPostTarget}}
	// Newest first, the last few.
	recent := slices.Clone(d.posts)
	slices.Reverse(recent)
	for i, m := range recent {
		if i == 12 {
			break
		}
		arrow := "← " + d.who(m.From)
		if m.From == "" {
			arrow = "→ " + d.who(m.To)
		}
		first, _, _ := strings.Cut(m.Text, "\n")
		items = append(items, menuItem{label: fit(first, 40), hint: arrow, run: func() tea.Cmd { d.menu = d.postMenu(m); return nil }})
	}
	if len(recent) == 0 {
		items = append(items, menuItem{label: "(no messages yet)", disabled: true})
	}
	m := newMenu("Pigeon Post", items...)
	m.note = []string{"Text between this terminal and the devices' browsers."}
	d.menu = m
	return nil
}

// pickPostTarget chooses who gets a message.
func (d *dashboard) pickPostTarget() tea.Cmd {
	var items []menuItem
	// Allowed devices, the ones with a page open first.
	devs := slices.Clone(d.devs)
	slices.SortStableFunc(devs, func(a, b devices.Device) int { return boolRank(d.online[b.IP] > 0) - boolRank(d.online[a.IP] > 0) })
	for _, dev := range devs {
		if dev.Status != devices.Allowed || dev.Local {
			continue
		}
		hint := "no page open"
		if n := d.online[dev.IP]; n > 0 {
			hint = fmt.Sprintf("%d tab(s)", n)
		}
		items = append(items, menuItem{label: dev.Label(), hint: hint, run: func() tea.Cmd { return d.composePost(dev.IP) }})
	}
	items = append(items, menuItem{label: "Every device", hint: "everyone with the page open", run: func() tea.Cmd { return d.composePost("") }})
	d.menu = newMenu("Send to whom?", items...)
	return nil
}

// boolRank turns a bool into a sort key.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}
