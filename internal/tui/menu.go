package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// menuItem is one choice in a popup menu.
type menuItem struct {
	label    string
	hint     string // short explanation shown after the label
	danger   bool   // drawn in the error colour (quit, cancel)
	disabled bool   // shown dimmed and skipped by the cursor
	run      func() tea.Cmd
}

// menu is a small modal list driven only by ↑/↓, enter and esc, so every
// dashboard action is reachable without memorising letter shortcuts.
type menu struct {
	title string
	items []menuItem
	cur   int
}

// newMenu builds a menu with the cursor on the first enabled item.
func newMenu(title string, items ...menuItem) *menu {
	m := &menu{title: title, items: items, cur: -1}
	m.move(1)
	return m
}

// move steps the cursor by delta, skipping disabled items and stopping at
// the ends of the list.
func (m *menu) move(delta int) {
	// Walk in the requested direction until an enabled item is found.
	for i := m.cur + delta; i >= 0 && i < len(m.items); i += delta {
		if !m.items[i].disabled {
			m.cur = i
			return
		}
	}
}

// selected returns the item under the cursor, if any.
func (m *menu) selected() (menuItem, bool) {
	if m.cur < 0 || m.cur >= len(m.items) {
		return menuItem{}, false
	}
	return m.items[m.cur], true
}

// view renders the menu as a bordered box.
func (m *menu) view(th theme) string {
	rows := make([]string, 0, len(m.items)+2)
	width := max(34, lipgloss.Width(m.title)+8)
	// Measure first so every row can be padded to the same width.
	for _, it := range m.items {
		w := 2 + lipgloss.Width(it.label)
		if it.hint != "" {
			w += 2 + lipgloss.Width(it.hint)
		}
		width = max(width, w+4)
	}
	// One row per item; the cursor row is highlighted.
	for i, it := range m.items {
		st := fgs(th.fg)
		switch {
		case it.disabled:
			st = fgs(th.faint)
		case it.danger:
			st = fgs(th.err)
		}
		prefix := "  "
		if i == m.cur {
			prefix = fgs(th.accent).Render("▸ ")
			st = st.Bold(true)
			if !it.danger {
				st = st.Foreground(th.accent)
			}
		}
		row := prefix + st.Render(it.label)
		if it.hint != "" {
			row += "  " + fgs(th.muted).Render(it.hint)
		}
		rows = append(rows, row)
	}
	rows = append(rows, "", fgs(th.faint).Render(strings.Join([]string{"↑↓ choose", "enter select", "esc back"}, " · ")))
	return th.panel(m.title, rows, width, true)
}

// overlay draws box centred on top of base (a full w×h screen).
func overlay(base, box string, w, h int) string {
	bw, bh := lipgloss.Size(box)
	x, y := max(0, (w-bw)/2), max(0, (h-bh)/2)
	return lipgloss.NewCompositor(
		lipgloss.NewLayer(base),
		lipgloss.NewLayer(box).X(x).Y(y).Z(1),
	).Render()
}
