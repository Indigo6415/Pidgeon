// Command pdgn is the short alias for pidgeon; it behaves identically.
package main

import (
	"os"

	"github.com/Indigo6415/pidgeon/internal/app"
)

func main() {
	os.Exit(app.Main(os.Args[1:], os.Stdout, os.Stderr))
}
