package tui

import (
	"encoding/json"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Indigo6415/pidgeon/internal/humanize"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// Delivery receipts in the dashboard: the bottom panel switches between the
// request log and the deliveries with "d", and the main menu lists them
// with their details.

// deliveriesPanel lists the most recent deliveries, newest first.
func (d *dashboard) deliveriesPanel(w, n int) string {
	th := d.th
	rs := d.o.Store.Receipts()
	title := fmt.Sprintf("Deliveries · %d · d: requests", d.o.Store.Deliveries())
	if len(rs) == 0 {
		return th.panel(title, []string{fgs(th.faint).Render("Nothing delivered yet. Every file or ZIP a device receives in full shows up here.")}, w, false)
	}
	inner := w - 4
	whoW := 22
	pathW := max(10, inner-10-whoW-11-15)
	var rows []string
	// One line per delivery: when, who, what, size, hash.
	for i, r := range rs {
		if i >= n {
			break
		}
		who := r.Client
		if r.Agent != "" {
			who = r.Agent
		}
		what := r.Path
		if r.Kind == "zip" {
			what += " (zip)"
		}
		if r.Share != "" {
			what += " (link)"
		}
		sum := "—"
		if r.SHA256 != "" {
			sum = r.SHA256[:12]
		}
		rows = append(rows,
			fgs(th.faint).Render(r.Time.Format("15:04:05")+"  ")+
				fgs(th.ok).Render("✓ ")+
				fgs(th.muted).Render(fit(who, whoW))+" "+
				fgs(th.fg).Render(fit(what, pathW))+" "+
				fgs(th.muted).Render(padRight(humanize.Bytes(r.Bytes), 10))+" "+
				fgs(th.faint).Render(sum))
	}
	return th.panel(title, rows, w, false)
}

// deliveriesMenu lists the receipts; each opens its details.
func (d *dashboard) deliveriesMenu() tea.Cmd {
	rs := d.o.Store.Receipts()
	var items []menuItem
	for _, r := range rs {
		who := r.Client
		if r.Agent != "" {
			who = r.Agent
		}
		items = append(items, menuItem{
			label: r.Time.Format("15:04") + "  " + fit(r.Path, 34),
			hint:  "→ " + who + " · " + humanize.Bytes(r.Bytes),
			run:   func() tea.Cmd { d.menu = d.receiptMenu(r); return nil },
		})
	}
	if len(items) == 0 {
		items = append(items, menuItem{label: "(nothing delivered yet)", disabled: true})
	}
	m := newMenu(fmt.Sprintf("Deliveries · %d", d.o.Store.Deliveries()), items...)
	m.note = []string{"Files and ZIPs that reached a device in full."}
	d.menu = m
	return nil
}

// receiptMenu shows one delivery and lets its hash or record be copied.
func (d *dashboard) receiptMenu(r stats.Receipt) *menu {
	who := r.Client
	if r.Agent != "" {
		who = r.Agent + " (" + r.Client + ")"
	}
	note := []string{
		r.Path,
		"to " + who + " · " + r.Time.Format("2006-01-02 15:04:05"),
		humanize.Bytes(r.Bytes) + " in " + humanize.Duration(time.Duration(r.Seconds*float64(time.Second))),
	}
	if r.Share != "" {
		note = append(note, "through a share link")
	}
	sha := menuItem{label: "Copy SHA-256", key: "c", run: func() tea.Cmd {
		d.say("copied the SHA-256 of "+r.Path, false)
		return tea.SetClipboard(r.SHA256)
	}}
	if r.SHA256 == "" {
		sha.disabled, sha.hint = true, "not known (resumed or changed)"
	} else {
		note = append(note, "SHA-256 "+r.SHA256)
	}
	line, _ := json.Marshal(r)
	m := newMenu("Delivery", sha,
		menuItem{label: "Copy as JSON", key: "j", run: func() tea.Cmd { d.say("copied the receipt", false); return tea.SetClipboard(string(line)) }},
		menuItem{label: "Back", run: d.deliveriesMenu},
	)
	m.note = wrapAll(note, menuMaxWidth-6)
	return m
}

// wrapAll wraps every line to w columns.
func wrapAll(lines []string, w int) []string {
	var out []string
	for _, l := range lines {
		out = append(out, wrapText(l, w, 4)...)
	}
	return out
}
