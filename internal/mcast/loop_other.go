//go:build !(darwin || dragonfly || freebsd || netbsd || openbsd || linux || solaris || illumos || aix || windows)

package mcast

import "net"

// setLoopback is a no-op where the option isn't available.
func setLoopback(*net.UDPConn) {}
