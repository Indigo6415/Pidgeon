//go:build windows

package mcast

import (
	"net"

	"golang.org/x/sys/windows"
)

// setLoopback turns multicast loopback on, so other programs on this
// machine (a second Pidgeon, `pidgeon find`) hear what this socket sends.
func setLoopback(c *net.UDPConn) {
	if rc, err := c.SyscallConn(); err == nil {
		rc.Control(func(fd uintptr) {
			windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, windows.IP_MULTICAST_LOOP, 1)
		})
	}
}
