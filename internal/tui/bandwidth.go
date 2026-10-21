package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/Indigo6415/pidgeon/internal/humanize"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// This file holds the dashboard's bandwidth controls: pausing, resuming
// and capping one transfer, and the caps on all downloads and per device.

// rateChoices are the speeds offered in the limit menus (bytes/sec).
var rateChoices = []int64{256 << 10, 1 << 20, 5 << 20, 10 << 20, 25 << 20, 50 << 20, 100 << 20}

// transferMenu lists the actions for one transfer: pause or resume, a
// speed cap, and cancel.
func (d *dashboard) transferMenu(t stats.View) *menu {
	back := menuItem{label: "Back", run: func() tea.Cmd { return nil }}
	hint := "the download fails on " + t.Client
	if t.Kind == stats.KindUpload {
		hint = "the upload from " + t.Client + " is discarded"
	}
	cancel := menuItem{label: "Cancel transfer", hint: hint, danger: true, run: d.cancelSelected}
	if t.State != stats.StateActive {
		cancel.disabled, cancel.hint = true, "already finished"
		return newMenu(t.Name, cancel, back)
	}
	pause := menuItem{label: "Pause", key: "p", hint: "holds it; the browser just waits", run: d.togglePause}
	if t.Paused {
		pause = menuItem{label: "Resume", key: "p", run: d.togglePause}
	}
	limitHint := "no cap of its own"
	if t.Limit > 0 {
		limitHint = "now " + humanize.Rate(float64(t.Limit))
	}
	id, name := t.ID, t.Name
	limit := menuItem{label: "Limit speed…", hint: limitHint, run: func() tea.Cmd {
		d.menu = d.rateMenu("Limit "+name, t.Limit, func(bps int64) {
			if !d.o.Store.SetTransferLimit(id, bps) {
				d.say("that transfer has already finished", true)
				return
			}
			d.say(capMessage(name, bps), false)
		})
		return nil
	}}
	return newMenu(t.Name, pause, limit, cancel, back)
}

// togglePause pauses or resumes the selected transfer.
func (d *dashboard) togglePause() tea.Cmd {
	i := d.selectedIndex()
	if d.focus != focusTransfers || i < 0 {
		d.say("move down to a transfer first, then press p to pause it", true)
		return nil
	}
	t := d.snap.Transfers[i]
	ok := false
	if t.Paused {
		ok = d.o.Store.Resume(t.ID)
		d.say("resumed "+t.Name+" → "+t.Client, false)
	} else {
		ok = d.o.Store.Pause(t.ID)
		d.say("paused "+t.Name+" → "+t.Client+" (p resumes)", false)
	}
	if !ok {
		d.say("that transfer has already finished", true)
	}
	d.refresh()
	return nil
}

// bandwidthMenu offers the caps on all downloads and per device.
func (d *dashboard) bandwidthMenu() *menu {
	st := d.o.Store
	total := menuItem{label: "Total speed limit…", hint: rateHint(st.Limit()), run: func() tea.Cmd {
		d.menu = d.rateMenu("Total download speed", st.Limit(), func(bps int64) {
			st.SetLimit(bps)
			d.say(capMessage("all downloads", bps), false)
		})
		return nil
	}}
	per := menuItem{label: "Per-device speed limit…", hint: rateHint(st.ClientLimit()), run: func() tea.Cmd {
		d.menu = d.rateMenu("Speed per device", st.ClientLimit(), func(bps int64) {
			st.SetClientLimit(bps)
			d.say(capMessage("each device", bps), false)
		})
		return nil
	}}
	m := newMenu("Bandwidth", total, per, menuItem{label: "Back", run: func() tea.Cmd { return nil }})
	m.note = []string{"Caps apply at once, also to running downloads."}
	return m
}

// rateMenu lets the operator pick a speed; set is called with the choice
// (0 for no limit). The current value is marked.
func (d *dashboard) rateMenu(title string, current int64, set func(int64)) *menu {
	items := make([]menuItem, 0, len(rateChoices)+2)
	// One item per preset speed, slowest first.
	for _, r := range rateChoices {
		it := menuItem{label: humanize.Rate(float64(r)), run: func() tea.Cmd { set(r); return nil }}
		if r == current {
			it.hint = "current"
		}
		items = append(items, it)
	}
	none := menuItem{label: "No limit", run: func() tea.Cmd { set(0); return nil }}
	if current == 0 {
		none.hint = "current"
	}
	items = append(items, none, menuItem{label: "Back", run: func() tea.Cmd { return nil }})
	m := newMenu(title, items...)
	// Start on the current choice, so enter keeps it.
	for i, it := range items {
		if it.hint == "current" {
			m.cur = i
		}
	}
	return m
}

// rateHint describes a cap for a menu hint.
func rateHint(bps int64) string {
	if bps <= 0 {
		return "none"
	}
	return humanize.Rate(float64(bps))
}

// capMessage is the footer message after a cap changes.
func capMessage(what string, bps int64) string {
	if bps <= 0 {
		return what + ": no speed limit"
	}
	return what + " limited to " + humanize.Rate(float64(bps))
}
