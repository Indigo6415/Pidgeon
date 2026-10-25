package tui

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"rsc.io/qr"
)

// qrQuiet is the white margin around the code, in modules. Two is enough
// for phone scanners because the block itself is drawn on white.
const qrQuiet = 2

// qrBlock is a rendered QR code plus its size in cells.
type qrBlock struct {
	text  string
	lines []string
	w     int
}

// renderQR encodes text and draws it with half-block characters, packing
// two module rows into each terminal row. Colours are forced to black on
// white so the code scans regardless of the terminal theme.
//
// Terminals with extra line spacing (Ghostty's adjust-cell-height, for
// one) draw block glyphs shorter than the cell, leaving a strip at the top
// that only the cell background covers. Solid cells are therefore drawn as
// background-coloured spaces, and mixed cells always as '▄' on the top
// half's colour, so the strip continues the top module in every column and
// module edges line up across the code instead of showing seams or steps.
func renderQR(text string) (qrBlock, bool) {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return qrBlock{}, false
	}
	n := code.Size + 2*qrQuiet
	dark := func(x, y int) bool {
		x, y = x-qrQuiet, y-qrQuiet
		// The quiet zone and the padding row below the code are light.
		if x < 0 || y < 0 || x >= code.Size || y >= code.Size {
			return false
		}
		return code.Black(x, y)
	}
	black, white := hex("#000000"), hex("#ffffff")
	col := func(d bool) color.Color {
		// Dark modules are black, everything else is white.
		if d {
			return black
		}
		return white
	}
	b := qrBlock{text: text, w: n}
	// Each output row covers module rows y (top half) and y+1 (bottom half).
	for y := 0; y < n; y += 2 {
		var sb strings.Builder
		for x := range n {
			top, bot := dark(x, y), dark(x, y+1)
			// Solid cells: a space on the matching background fills the
			// whole cell, line spacing included.
			if top == bot {
				sb.WriteString(lipgloss.NewStyle().Background(col(top)).Render(" "))
				continue
			}
			// Mixed cells: the background (top colour) also fills the strip
			// above the glyph, which belongs to the top module anyway.
			sb.WriteString(lipgloss.NewStyle().Foreground(col(bot)).Background(col(top)).Render("▄"))
		}
		b.lines = append(b.lines, sb.String())
	}
	return b, true
}
