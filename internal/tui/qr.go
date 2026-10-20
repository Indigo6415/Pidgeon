package tui

import (
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
	st := lipgloss.NewStyle().Foreground(hex("#000000")).Background(hex("#ffffff"))
	b := qrBlock{text: text, w: n}
	// Each output row covers module rows y (top half) and y+1 (bottom half).
	for y := 0; y < n; y += 2 {
		var sb strings.Builder
		for x := range n {
			top, bot := dark(x, y), dark(x, y+1)
			switch {
			case top && bot:
				sb.WriteRune('█')
			case top:
				sb.WriteRune('▀')
			case bot:
				sb.WriteRune('▄')
			default:
				sb.WriteRune(' ')
			}
		}
		b.lines = append(b.lines, st.Render(sb.String()))
	}
	return b, true
}
