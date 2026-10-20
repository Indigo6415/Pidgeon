// Package humanize formats byte counts, rates and durations for people.
package humanize

import (
	"fmt"
	"math"
	"time"
)

var units = []string{"B", "KB", "MB", "GB", "TB", "PB"}

// Bytes formats n using 1024-based units with up to 3 significant digits,
// e.g. "512 B", "1.24 MB", "38.1 GB".
func Bytes(n int64) string {
	// Negative means "unknown" throughout the codebase.
	if n < 0 {
		return "?"
	}
	v := float64(n)
	i := 0
	// Divide down until the value fits in the current unit.
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	switch {
	case i == 0:
		return fmt.Sprintf("%d B", n)
	case v < 10:
		return fmt.Sprintf("%.2f %s", v, units[i])
	case v < 100:
		return fmt.Sprintf("%.1f %s", v, units[i])
	}
	return fmt.Sprintf("%.0f %s", v, units[i])
}

// Rate formats bytes per second.
func Rate(bps float64) string {
	// NaN/Inf can appear transiently from a zero interval; show zero.
	if math.IsNaN(bps) || math.IsInf(bps, 0) || bps < 0 {
		bps = 0
	}
	return Bytes(int64(bps)) + "/s"
}

// Duration formats d compactly: "45s", "3m05s", "2h07m", "3d04h".
func Duration(d time.Duration) string {
	d = d.Round(time.Second)
	s := int64(d.Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	case s < 86400:
		return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
	}
	return fmt.Sprintf("%dd%02dh", s/86400, (s%86400)/3600)
}

// ETA estimates the time remaining, or "" when it can't be estimated.
func ETA(sent, total int64, bps float64) string {
	// Unknown size or stalled transfer: no meaningful estimate.
	if total <= 0 || bps < 1 || sent >= total {
		return ""
	}
	return Duration(time.Duration(float64(total-sent) / bps * float64(time.Second)))
}
