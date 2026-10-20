//go:build !unix

package app

import "os"

// inForeground always reports true: there are no background process groups
// that the terminal can stop outside Unix.
func inForeground(*os.File) bool { return true }
