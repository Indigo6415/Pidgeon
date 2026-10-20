// Package brand is the single source of truth for the product's name.
//
// To rename the tool, edit the constants below and rename the two
// directories under cmd/ to match Command and Alias (Go names a binary
// after its main package's directory, so `go install ./...` needs them).
package brand

import (
	"regexp"
	"runtime/debug"
)

const (
	// Name is the human-facing product name used in the TUI, web UI and docs.
	Name = "Pidgeon"

	// Command is the primary binary name.
	Command = "pidgeon"

	// Alias is the short binary name; it behaves identically to Command.
	Alias = "pdgn"

	// Tagline is shown on the splash screen and in --help.
	Tagline = "Carrier pidgeons delivering bytes to you."

	// CookieName is the cookie that stores the access token in the browser.
	CookieName = Command + "_token"
)

// Version is overridden at build time by the Makefile via
// -ldflags "-X github.com/Indigo6415/pidgeon/internal/brand.Version=...".
var Version = "dev"

// ResolvedVersion returns Version, falling back to the module version that
// `go install pkg@version` records in the binary's build info.
func ResolvedVersion() string {
	// Prefer the explicitly injected version when the Makefile set one.
	if Version != "dev" {
		return Version
	}
	// `go install ...@vX.Y.Z` embeds the module version; use it if present.
	// Local builds get VCS pseudo-versions ("v0.0.0-2026…-abc+dirty"); those
	// are noise, so only clean tagged versions are shown.
	if info, ok := debug.ReadBuildInfo(); ok && taggedVersion.MatchString(info.Main.Version) {
		return info.Main.Version
	}
	return Version
}

// taggedVersion matches vX.Y.Z with an optional short pre-release, but not
// Go's pseudo-versions or +dirty builds.
var taggedVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.]{1,12})?$`)
