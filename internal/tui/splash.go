package tui

// This file holds the whole terminal splash: hexagon geometry, palette and
// the animation timeline. The bird itself lives in internal/brand/art.go so
// the web splash draws the very same pigeon; redraw it there.

import (
	"image/color"
	"math"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/Indigo6415/pidgeon/internal/brand"
)

// Splash timeline. Total ≈ 1.25s: pop-in, one comet lap, a flash as the
// comet "lands", then a quantised fade to the terminal background.
const (
	popDur    = 140 * time.Millisecond
	cometDur  = 620 * time.Millisecond
	flashDur  = 60 * time.Millisecond
	fadeSteps = 10
	fadeStep  = 45 * time.Millisecond

	cometStart = popDur
	flashStart = cometStart + cometDur
	fadeStart  = flashStart + flashDur
	splashEnd  = fadeStart + fadeSteps*fadeStep

	splashFrame = time.Second / 60 // tick rate while the splash runs
	trailLen    = 16               // comet tail length in perimeter cells
)

// hexSizes are the pop-in frames (half-height n, top width s); the last
// entry is the final size. The bird appears once the frame is full size.
var hexSizes = [][2]int{{3, 9}, {4, 12}, {5, 15}, {6, 18}}

// cell is one character on the splash canvas.
type cell struct {
	r rune
	c color.Color
}

// canvas is a fixed-size grid of cells.
type canvas struct {
	w, h  int
	cells [][]cell
}

// newCanvas allocates an empty w×h canvas.
func newCanvas(w, h int) *canvas {
	c := &canvas{w: w, h: h, cells: make([][]cell, h)}
	// Every row starts as spaces with no colour.
	for y := range c.cells {
		c.cells[y] = make([]cell, w)
		for x := range c.cells[y] {
			c.cells[y][x] = cell{r: ' '}
		}
	}
	return c
}

// set writes one cell, ignoring out-of-bounds coordinates.
func (c *canvas) set(x, y int, r rune, col color.Color) {
	if x >= 0 && y >= 0 && x < c.w && y < c.h {
		c.cells[y][x] = cell{r: r, c: col}
	}
}

// text writes a string starting at (x, y) in one colour.
func (c *canvas) text(x, y int, s string, col color.Color) {
	// Rune-by-rune so each lands in its own cell.
	for i, r := range []rune(s) {
		c.set(x+i, y, r, col)
	}
}

// render turns the canvas into styled lines, merging runs of equal colour
// into one escape sequence so frames stay small and flicker-free.
func (c *canvas) render() string {
	var out strings.Builder
	// Emit each row as a sequence of same-coloured runs.
	for y, row := range c.cells {
		if y > 0 {
			out.WriteByte('\n')
		}
		var run strings.Builder
		var runCol color.Color
		flush := func() {
			// Colourless runs are plain spaces; skip styling them.
			if run.Len() == 0 {
				return
			}
			if runCol == nil {
				out.WriteString(run.String())
			} else {
				out.WriteString(lipgloss.NewStyle().Foreground(runCol).Render(run.String()))
			}
			run.Reset()
		}
		for _, cl := range row {
			col := cl.c
			// Spaces don't need a colour; fold them into any run.
			if cl.r == ' ' {
				col = runCol
			}
			if !sameColor(col, runCol) {
				flush()
				runCol = col
			}
			run.WriteRune(cl.r)
		}
		flush()
	}
	return out.String()
}

// sameColor compares two (possibly nil) colours.
func sameColor(a, b color.Color) bool {
	if a == nil || b == nil {
		return a == b
	}
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	return ar == br && ag == bg && ab == bb
}

// perimCell is one cell of the hexagon outline, in clockwise order.
type perimCell struct {
	x, y int
	r    rune
}

// hexagon returns the outline of a flat-topped hexagon of half-height n and
// top-edge width s, clockwise from the top-left of the top edge, plus its
// total width. Diagonals use ╱╲; the flat edges use ▔ / ▁ so they meet the
// diagonals' tips exactly at the cell corners.
func hexagon(n, s int) ([]perimCell, int) {
	w := 2*n + s
	var p []perimCell
	// Top edge, left → right.
	for x := n; x < n+s; x++ {
		p = append(p, perimCell{x, 0, '▔'})
	}
	// Upper-right edge, top → middle.
	for r := range n {
		p = append(p, perimCell{w - n + r, r, '╲'})
	}
	// Lower-right edge, middle → bottom.
	for k := range n {
		p = append(p, perimCell{w - 1 - k, n + k, '╱'})
	}
	// Bottom edge, right → left.
	for x := n + s - 1; x >= n; x-- {
		p = append(p, perimCell{x, 2*n - 1, '▁'})
	}
	// Lower-left edge, bottom → middle.
	for k := n - 1; k >= 0; k-- {
		p = append(p, perimCell{k, n + k, '╲'})
	}
	// Upper-left edge, middle → top.
	for r := n - 1; r >= 0; r-- {
		p = append(p, perimCell{n - 1 - r, r, '╱'})
	}
	return p, w
}

// splash is the animation state. It only tracks time; every frame is a pure
// function of elapsed time, so dropped frames never desynchronise it.
type splash struct {
	start time.Time
	urls  []string // listen URLs, faded in as the splash fades out
}

// splashTickMsg drives the splash animation.
type splashTickMsg time.Time

// done reports whether the animation has finished.
func (s splash) done(now time.Time) bool { return now.Sub(s.start) >= splashEnd }

// blockSize returns the width and height of the whole splash block, used
// both for layout and to skip the splash on terminals too small for it.
func (s splash) blockSize() (int, int) {
	n, sz := hexSizes[len(hexSizes)-1][0], hexSizes[len(hexSizes)-1][1]
	w := max(2*n+sz, lipgloss.Width(brand.Tagline))
	h := 2*n + 3 // hexagon + blank + title + tagline
	// Room for the URLs that fade in at the end.
	if len(s.urls) > 0 {
		h += 1 + len(s.urls)
	}
	for _, u := range s.urls {
		w = max(w, lipgloss.Width(u)+4)
	}
	return w, h
}

// view renders the splash frame for time now, centred in a w×h screen.
func (s splash) view(now time.Time, w, h int, th theme) string {
	t := now.Sub(s.start)
	bg := th.bg

	// Base palette; everything below is blended toward bg during the fade.
	frameCol := lerp(th.accent, bg, 0.55)
	// The bird is coloured with VS Code syntax colours, as in the web splash.
	body := th.fg
	palette := map[byte]color.Color{
		' ': body,
		'e': hex("#4fc1ff"), // function blue
		'k': hex("#ce9178"), // string orange
		'n': th.accent2,     // keyword purple
		'w': hex("#9cdcfe"), // variable light blue
		'f': hex("#d7ba7d"), // escape gold
	}
	if !th.dark {
		palette['e'], palette['k'], palette['w'], palette['f'] = hex("#0070c1"), hex("#a31515"), hex("#001080"), hex("#795e26")
	}

	// Fade factor: 0 until the fade starts, then 10 discrete steps to 1.
	fade := 0.0
	if t >= fadeStart {
		step := int((t-fadeStart)/fadeStep) + 1
		fade = math.Min(1, float64(step)/fadeSteps)
	}
	dim := func(c color.Color) color.Color { return lerp(c, bg, fade) }

	// Pop-in: pick the hexagon size for the current moment.
	sizeIdx := len(hexSizes) - 1
	if t < popDur {
		sizeIdx = min(len(hexSizes)-1, int(t*time.Duration(len(hexSizes))/popDur))
	}
	n, sz := hexSizes[sizeIdx][0], hexSizes[sizeIdx][1]
	fullN, fullS := hexSizes[len(hexSizes)-1][0], hexSizes[len(hexSizes)-1][1]
	fullW, fullH := 2*fullN+fullS, 2*fullN

	blockW, blockH := s.blockSize()
	cv := newCanvas(blockW, blockH)
	hexX := (blockW - fullW) / 2

	// Draw the outline, centred on the final hexagon's centre.
	perim, pw := hexagon(n, sz)
	ox := hexX + (fullW-pw)/2
	oy := (fullH - 2*n) / 2
	headPos := -1.0
	// During the lap, the comet head moves along the perimeter with an
	// ease-in-out curve so it accelerates away and settles home.
	if t >= cometStart && t < flashStart {
		u := float64(t-cometStart) / float64(cometDur)
		headPos = easeInOutCubic(u) * float64(len(perim))
	}
	flash := 0.0
	// The flash decays from full highlight back to the base colour.
	if t >= flashStart && t < fadeStart+2*fadeStep {
		flash = 1 - float64(t-flashStart)/float64(flashDur+2*fadeStep)
	}
	head := hex("#f4fffe")
	if !th.dark {
		head = hex("#06302c")
	}
	for i, pc := range perim {
		col := frameCol
		// Cells behind the head glow, fading along the tail: white → teal → violet.
		if headPos >= 0 {
			d := headPos - float64(i)
			if d < 0 {
				d += float64(len(perim))
			}
			if d < trailLen {
				f := d / trailLen
				var glow color.Color
				if f < 0.35 {
					glow = lerp(head, th.accent, f/0.35)
				} else {
					glow = lerp(th.accent, th.accent2, (f-0.35)/0.65)
				}
				col = lerp(glow, frameCol, math.Pow(f, 1.6))
			}
		}
		if flash > 0 {
			col = lerp(col, th.accent, flash)
		}
		cv.set(ox+pc.x, oy+pc.y, pc.r, dim(col))
	}

	// The bird (only once the frame is full size), centred in the hexagon.
	if sizeIdx == len(hexSizes)-1 {
		artW := 0
		for _, l := range brand.PigeonArt {
			artW = max(artW, len([]rune(l)))
		}
		ax := hexX + (fullW-artW)/2
		ay := (fullH - len(brand.PigeonArt)) / 2
		// Fade the bird in over the first part of the comet lap.
		in := math.Min(1, float64(t-popDur+40*time.Millisecond)/float64(120*time.Millisecond))
		for y, line := range brand.PigeonArt {
			mask := []rune("")
			if y < len(brand.PigeonMask) {
				mask = []rune(brand.PigeonMask[y])
			}
			for x, r := range []rune(line) {
				if r == ' ' {
					continue
				}
				m := byte(' ')
				if x < len(mask) {
					m = byte(mask[x])
				}
				col, ok := palette[m]
				if !ok {
					col = body
				}
				cv.set(ax+x, ay+y, r, dim(lerp(bg, col, in)))
			}
		}
	}

	// Title and tagline under the hexagon.
	title := spaced(strings.ToUpper(brand.Name))
	titleCol := th.fg
	if flash > 0 {
		titleCol = lerp(th.fg, th.accent, flash)
	}
	cv.text((blockW-len([]rune(title)))/2, fullH+1, title, dim(titleCol))
	cv.text((blockW-len([]rune(brand.Tagline)))/2, fullH+2, brand.Tagline, dim(th.muted))

	// Listen URLs fade IN while everything else fades out, so they're never
	// hidden by the splash and are on screen when the dashboard takes over.
	if len(s.urls) > 0 && t >= fadeStart-2*fadeStep {
		in := math.Min(1, fade+0.2)
		for i, u := range s.urls {
			line := "➜ " + u
			cv.text((blockW-len([]rune(line)))/2, fullH+4+i, line, lerp(bg, th.accent, in))
		}
	}

	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, cv.render())
}

// spaced inserts a space between letters: "PIDGEON" → "P I D G E O N".
func spaced(s string) string {
	return strings.Join(strings.Split(s, ""), " ")
}

// easeInOutCubic maps 0..1 to 0..1 with a smooth start and finish.
func easeInOutCubic(u float64) float64 {
	u = math.Min(1, math.Max(0, u))
	if u < 0.5 {
		return 4 * u * u * u
	}
	return 1 - math.Pow(-2*u+2, 3)/2
}
