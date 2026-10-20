// Package pull is the (planned) `pidgeon pull <url> [dir]` subcommand.
//
// Browsers decide where downloads are saved, so the web UI preserves folder
// structure by streaming a ZIP. To write files straight into a chosen
// directory on the receiving machine, the receiver needs its own client:
// that is what `pull` will be. The planned design:
//
//  1. GET <url>/api/ls?path=... recursively to discover the tree (the same
//     API the web UI uses; pass --token through as a Bearer header).
//  2. Recreate directories under the target dir, refusing any entry whose
//     cleaned path would escape it.
//  3. GET /f/<path> for each file, writing to "<name>.part" and renaming on
//     success, with Range requests to resume partial files.
//  4. Run a few downloads in parallel and show one progress line per file.
//
// It is a stub so the main build doesn't depend on it.
package pull

import (
	"flag"
	"fmt"
	"io"
	"net/url"

	"github.com/Indigo6415/pidgeon/internal/brand"
)

// Main parses the pull arguments and reports that the feature is pending.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(brand.Command+" pull", flag.ContinueOnError)
	fs.SetOutput(stderr)
	token := fs.String("token", "", "access token for a protected server")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s pull [--token T] <url> [target-dir]\n\n", brand.Command)
		fmt.Fprintln(stderr, "Download a folder from another "+brand.Name+" server into target-dir,")
		fmt.Fprintln(stderr, "keeping its structure. (Not implemented yet.)")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Exactly one URL plus an optional target directory.
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	u, err := url.Parse(fs.Arg(0))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		fmt.Fprintf(stderr, "%s pull: %q is not an http(s) URL\n", brand.Command, fs.Arg(0))
		return 2
	}
	target := "."
	if fs.NArg() == 2 {
		target = fs.Arg(1)
	}
	_ = token
	fmt.Fprintf(stderr, "%s pull is not implemented yet (would fetch %s into %s).\n", brand.Command, u, target)
	fmt.Fprintln(stderr, "For now, select files in the web UI and download them as a ZIP.")
	return 3
}
