// Package web embeds the browser UI so the binary is self-contained.
package web

import (
	"embed"
	"io/fs"
)

//go:embed assets
var assets embed.FS

// Assets returns the web UI files rooted at the assets directory.
func Assets() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	// fs.Sub only fails for an invalid path, which is a build-time constant.
	if err != nil {
		panic(err)
	}
	return sub
}
