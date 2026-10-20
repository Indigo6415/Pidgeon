//go:build unix

package app

import (
	"os"

	"golang.org/x/sys/unix"
)

// inForeground reports whether this process owns the terminal f, i.e. it was
// not started with `&` or moved to the background. A background process that
// touches the terminal gets stopped by SIGTTIN/SIGTTOU, so it must run headless.
func inForeground(f *os.File) bool {
	pgrp, err := unix.IoctlGetInt(int(f.Fd()), unix.TIOCGPGRP)
	// Not a controlling terminal: nothing can stop us, so treat as foreground.
	if err != nil {
		return true
	}
	return pgrp == unix.Getpgrp()
}
