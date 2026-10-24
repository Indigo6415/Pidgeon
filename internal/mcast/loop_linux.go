//go:build linux || solaris || illumos || aix

package mcast

import (
	"net"

	"golang.org/x/sys/unix"
)

// setLoopback turns multicast loopback on, so other programs on this
// machine (a second Pidgeon, `pidgeon find`) hear what this socket sends.
func setLoopback(c *net.UDPConn) {
	if rc, err := c.SyscallConn(); err == nil {
		rc.Control(func(fd uintptr) { unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MULTICAST_LOOP, 1) })
	}
}
