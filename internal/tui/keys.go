package tui

import "charm.land/bubbles/v2/key"

// keyMap lists every dashboard binding; the footer is generated from it.
// Everything is reachable with the arrows, enter and esc alone; the letter
// keys are shortcuts for the same actions. Approval prompts also answer to
// y / a / n while they are open.
type keyMap struct {
	Up, Down, Enter, Actions, Back, Menu          key.Binding
	Toggle, Focus, Cancel, Copy, Open, Help, Quit key.Binding
	Share, Pause, Post, Deliveries                key.Binding
}

// defaultKeys returns the dashboard key bindings.
func defaultKeys() keyMap {
	return keyMap{
		Up:      key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑", "up")),
		Down:    key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓", "down")),
		Enter:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "toggle / select")),
		Actions: key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→", "actions")),
		Back:    key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←", "back")),
		Menu:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "menu")),

		Toggle: key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "toggle listener")),
		Focus:  key.NewBinding(key.WithKeys("tab", "shift+tab"), key.WithHelp("tab", "switch panel")),
		Cancel: key.NewBinding(key.WithKeys("x", "delete"), key.WithHelp("x", "cancel transfer")),
		Copy:   key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy URL")),
		Open:   key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open in browser")),
		Share:  key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "new share link")),
		Pause:  key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pause / resume transfer")),
		Post:   key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "pigeon post (messages)")),

		Deliveries: key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "requests / deliveries")),
		Help:       key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "all keys")),
		Quit:       key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

// ShortHelp implements help.KeyMap (footer line): the arrow/enter/esc set.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Enter, k.Actions, k.Menu}
}

// FullHelp implements help.KeyMap (shown from the esc menu or with "?").
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Enter},
		{k.Actions, k.Back, k.Menu},
		{k.Toggle, k.Focus, k.Cancel},
		{k.Pause, k.Copy, k.Open},
		{k.Share, k.Post, k.Deliveries},
		{k.Help, k.Quit},
	}
}
