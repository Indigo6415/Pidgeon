//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package mcast

import (
	"net"

	"golang.org/x/sys/unix"
)

// setLoopback turns multicast loopback on, so other programs on this
// machine (a second Pidgeon, `pidgeon find`) hear what this socket sends.
// BSDs take the option as a single byte.
func setLoopback(c *net.UDPConn) {
	if rc, err := c.SyscallConn(); err == nil {
		rc.Control(func(fd uintptr) { unix.SetsockoptByte(int(fd), unix.IPPROTO_IP, unix.IP_MULTICAST_LOOP, 1) })
	}
}
