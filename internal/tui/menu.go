package tui

import (
	"fmt"
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
	key      string // optional one-key shortcut while the menu is open ("y", "n")
	run      func() tea.Cmd
}

// menu is a small modal list driven only by ↑/↓, enter and esc, so every
// dashboard action is reachable without memorising letter shortcuts.
type menu struct {
	title string
	note  []string // optional lines of context shown above the items
	items []menuItem
	cur   int

	// back, if set, handles ← instead of closing the menu (the file
	// picker uses it to go up a folder).
	back func() tea.Cmd
	// prompt is the approval this menu answers, 0 for ordinary menus.
	prompt uint64
}

// menuRows is how many items a menu shows at once; longer menus scroll.
const menuRows = 14

// menuMaxWidth caps a menu's width so long file names don't fill the screen.
const menuMaxWidth = 72

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

// shortcut returns the enabled item bound to key k, if any.
func (m *menu) shortcut(k string) (menuItem, bool) {
	// Shortcuts are few; scan them all.
	for _, it := range m.items {
		if it.key != "" && it.key == k && !it.disabled {
			return it, true
		}
	}
	return menuItem{}, false
}

// view renders the menu as a bordered box.
func (m *menu) view(th theme) string {
	rows := make([]string, 0, min(len(m.items), menuRows)+len(m.note)+4)
	width := max(34, lipgloss.Width(m.title)+8)
	// Measure first so every row can be padded to the same width.
	for _, it := range m.items {
		w := 2 + lipgloss.Width(it.label)
		if it.hint != "" {
			w += 2 + lipgloss.Width(it.hint)
		}
		if it.key != "" {
			w += 4
		}
		width = max(width, w+4)
	}
	for _, n := range m.note {
		width = max(width, lipgloss.Width(n)+4)
	}
	width = min(width, menuMaxWidth)
	// Context lines first, separated from the choices by a blank line.
	for _, n := range m.note {
		rows = append(rows, fgs(th.muted).Render(n))
	}
	if len(m.note) > 0 {
		rows = append(rows, "")
	}
	// Long menus show a window around the cursor.
	start, end := 0, len(m.items)
	if len(m.items) > menuRows {
		start = min(max(0, m.cur-menuRows/2), len(m.items)-menuRows)
		end = start + menuRows
		if start > 0 {
			rows = append(rows, fgs(th.faint).Render(fmt.Sprintf("  ↑ %d more", start)))
		}
	}
	// One row per item; the cursor row is highlighted.
	for i := start; i < end; i++ {
		it := m.items[i]
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
		if it.key != "" {
			row += fgs(th.faint).Render(" (" + it.key + ")")
		}
		if it.hint != "" {
			row += "  " + fgs(th.muted).Render(it.hint)
		}
		rows = append(rows, row)
	}
	if end < len(m.items) {
		rows = append(rows, fgs(th.faint).Render(fmt.Sprintf("  ↓ %d more", len(m.items)-end)))
	}
	help := []string{"↑↓ choose", "enter select", "esc back"}
	if m.back != nil {
		help = []string{"↑↓ choose", "enter open", "← up", "esc close"}
	}
	rows = append(rows, "", fgs(th.faint).Render(strings.Join(help, " · ")))
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
