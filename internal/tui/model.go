// Package tui is the terminal dashboard: a short splash, then a live view of
// listeners, transfers, throughput and requests with listener controls.
package tui

import (
	"context"
	"errors"
	"image/color"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	tea "charm.land/bubbletea/v2"

	"github.com/Indigo6415/pidgeon/internal/brand"
	"github.com/Indigo6415/pidgeon/internal/devices"
	"github.com/Indigo6415/pidgeon/internal/humanize"
	"github.com/Indigo6415/pidgeon/internal/listeners"
	"github.com/Indigo6415/pidgeon/internal/server"
	"github.com/Indigo6415/pidgeon/internal/stats"
)

// dashEvery is the dashboard refresh period (8 Hz).
const dashEvery = 125 * time.Millisecond

// Options configures the TUI.
type Options struct {
	Ctx      context.Context // cancelled on SIGTERM/SIGINT: quits the TUI
	Store    *stats.Store
	Manager  *listeners.Manager
	Dir      string // absolute served directory
	Token    string // appended to shared URLs (QR, copy, open) when set
	AuthMode string // "open", "token", "basic", "token + basic"
	Splash   bool   // play the startup splash
	URLs     []string

	Server  *server.Server    // share links, sending files, the file picker; optional
	Devices *devices.Registry // the Devices panel and --ask approvals; optional
	Limits  Limits            // shown as a countdown / budget in the header
}

// Limits are the conditions under which the session ends by itself.
type Limits struct {
	Deadline time.Time // --expire: stop at this time (zero = never)
	Once     bool      // --once: stop after the first completed download
	MaxBytes int64     // --max-bytes: stop once this much has been served (0 = no limit)
}

// Active reports whether any limit is set.
func (l Limits) Active() bool { return !l.Deadline.IsZero() || l.Once || l.MaxBytes > 0 }

// Describe says when the session ends, e.g. "in 30m or after 5.00 GB".
func (l Limits) Describe() string {
	var parts []string
	if !l.Deadline.IsZero() {
		parts = append(parts, "in "+humanize.Duration(time.Until(l.Deadline)))
	}
	if l.Once {
		parts = append(parts, "after the first download")
	}
	if l.MaxBytes > 0 {
		parts = append(parts, "after "+humanize.Bytes(l.MaxBytes)+" served")
	}
	return strings.Join(parts, " or ")
}

// Run starts the dashboard and blocks until the user quits or Ctx ends.
// The server is already serving when this is called; the TUI is cosmetic.
func Run(o Options) error {
	m := newModel(o)
	p := tea.NewProgram(m, tea.WithContext(o.Ctx), tea.WithoutSignalHandler())
	_, err := p.Run()
	// A cancelled context is the normal SIGTERM path, not a failure.
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// phase is which screen the root model shows.
type phase int

const (
	phaseSplash phase = iota
	phaseDash
)

// dashTickMsg drives dashboard refreshes.
type dashTickMsg time.Time

// model is the root Bubble Tea model. The splash is just its initial state;
// when the animation ends it switches to the dashboard in place.
type model struct {
	o     Options
	phase phase
	w, h  int
	th    theme

	splash splash
	dash   *dashboard
}

// newModel builds the initial model.
func newModel(o Options) model {
	// Assume a dark terminal until it reports its real background colour.
	th := newTheme(true, color.RGBA{0, 0, 0, 0xff})
	m := model{o: o, th: th, dash: newDashboard(o)}
	m.phase = phaseDash
	if o.Splash {
		m.phase = phaseSplash
		m.splash = splash{start: time.Now(), urls: o.URLs}
	}
	return m
}

// Init asks for the terminal background (used by the fade and theme) and
// starts the tick loop appropriate to the current phase.
func (m model) Init() tea.Cmd {
	if m.phase == phaseSplash {
		return tea.Batch(tea.RequestBackgroundColor, splashTick())
	}
	return tea.Batch(tea.RequestBackgroundColor, dashTick())
}

// splashTick schedules the next splash frame.
func splashTick() tea.Cmd {
	return tea.Tick(splashFrame, func(t time.Time) tea.Msg { return splashTickMsg(t) })
}

// dashTick schedules the next dashboard refresh.
func dashTick() tea.Cmd {
	return tea.Tick(dashEvery, func(t time.Time) tea.Msg { return dashTickMsg(t) })
}

// toDash hands off from the splash: clear the screen so no splash cell
// survives, then start the dashboard's own tick loop.
func (m model) toDash() (tea.Model, tea.Cmd) {
	m.phase = phaseDash
	m.dash.refresh()
	return m, tea.Sequence(tea.ClearScreen, dashTick())
}

// Update routes messages to the active phase.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.dash.help.SetWidth(msg.Width)
		// A terminal too small for the splash skips straight to the dashboard.
		if m.phase == phaseSplash {
			if bw, bh := m.splash.blockSize(); m.w < bw+2 || m.h < bh+2 {
				return m.toDash()
			}
		}
		return m, nil

	case tea.BackgroundColorMsg:
		m.th = newTheme(msg.IsDark(), msg.Color)
		m.dash.th = m.th
		m.dash.help.Styles = help.DefaultStyles(msg.IsDark())
		return m, nil

	case splashTickMsg:
		// Ticks that arrive after a skip are simply dropped.
		if m.phase != phaseSplash {
			return m, nil
		}
		if m.splash.done(time.Time(msg)) {
			return m.toDash()
		}
		return m, splashTick()

	case dashTickMsg:
		if m.phase != phaseDash {
			return m, nil
		}
		m.dash.refresh()
		return m, dashTick()

	case tea.KeyPressMsg:
		// Any key skips the splash; ctrl+c still quits immediately.
		if m.phase == phaseSplash {
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m.toDash()
		}
		return m, m.dash.handleKey(msg)
	}
	return m, nil
}

// View renders the active phase on the alternate screen.
func (m model) View() tea.View {
	var content string
	// Nothing sensible can be drawn before the first size message.
	if m.w > 0 && m.h > 0 {
		if m.phase == phaseSplash {
			content = m.splash.view(time.Now(), m.w, m.h, m.th)
		} else {
			content = m.dash.view(m.w, m.h)
		}
	}
	v := tea.NewView(content)
	v.AltScreen = true
	v.WindowTitle = brand.Name + " · " + m.o.Dir
	return v
}
