package tui

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// theme is the dashboard palette, taken from VS Code's Dark Modern / Light
// Modern themes (accent = link blue, accent2 = keyword purple) so the
// terminal matches the web UI.
type theme struct {
	dark                   bool
	bg                     color.Color
	fg, muted, faint, line color.Color
	accent, accent2        color.Color
	ok, warn, err          color.Color
	selBg                  color.Color
}

// newTheme returns the palette for a dark or light terminal background.
func newTheme(dark bool, bg color.Color) theme {
	if dark {
		return theme{
			dark: true, bg: bg,
			fg: hex("#cccccc"), muted: hex("#9d9d9d"), faint: hex("#6e7681"), line: hex("#3c3c3c"),
			accent: hex("#4daafc"), accent2: hex("#c586c0"),
			ok: hex("#89d185"), warn: hex("#cca700"), err: hex("#f85149"),
			selBg: hex("#04395e"),
		}
	}
	return theme{
		dark: false, bg: bg,
		fg: hex("#3b3b3b"), muted: hex("#6f6f6f"), faint: hex("#9a9a9a"), line: hex("#cecece"),
		accent: hex("#005fb8"), accent2: hex("#af00db"),
		ok: hex("#388a34"), warn: hex("#bf8803"), err: hex("#e51400"),
		selBg: hex("#e8e8e8"),
	}
}

// hex parses a "#rrggbb" color.
func hex(s string) color.Color { return lipgloss.Color(s) }

// fgs returns a style with only a foreground color.
func fgs(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }

// panel draws a rounded box with an inline title. Every body line is
// truncated/padded to exactly fit, so the box height is len(body)+2 and the
// layout math in the dashboard stays exact.
func (t theme) panel(title string, body []string, width int, focused bool) string {
	// Too narrow to draw a box at all.
	if width < 6 {
		return ""
	}
	bc := t.line
	tc := fgs(t.fg).Bold(true)
	if focused {
		bc = t.accent
		tc = fgs(t.accent).Bold(true)
	}
	b := fgs(bc)
	inner := width - 4

	var sb strings.Builder
	head := " " + title + " "
	fill := max(0, width-3-lipgloss.Width(head))
	sb.WriteString(b.Render("╭─") + tc.Render(head) + b.Render(strings.Repeat("─", fill)+"╮") + "\n")
	// Each body line is fitted to the inner width.
	for _, line := range body {
		sb.WriteString(b.Render("│ ") + fit(line, inner) + b.Render(" │") + "\n")
	}
	sb.WriteString(b.Render("╰" + strings.Repeat("─", width-2) + "╯"))
	return sb.String()
}

// fit truncates (ANSI-aware) or pads s to exactly w cells.
func fit(s string, w int) string {
	// Overlong: cut with an ellipsis; short: pad with spaces.
	if lipgloss.Width(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", max(0, w-lipgloss.Width(s)))
}

// padRight pads plain or styled text to w cells without truncating.
func padRight(s string, w int) string {
	return s + strings.Repeat(" ", max(0, w-lipgloss.Width(s)))
}

// lerp blends a toward b by f (0 = a, 1 = b) in sRGB. Good enough for short
// UI fades; the steps are quantised anyway.
func lerp(a, b color.Color, f float64) color.Color {
	f = min(1, max(0, f))
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	mix := func(x, y uint32) uint8 {
		return uint8((float64(x>>8)*(1-f) + float64(y>>8)*f) + 0.5)
	}
	return color.RGBA{R: mix(ar, br), G: mix(ag, bg), B: mix(ab, bb), A: 0xff}
}
