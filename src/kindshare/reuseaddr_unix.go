//go:build !windows

package main

import "syscall"

// reuseAddr lets our mDNS socket share port 5353 with whatever else on the
// device already has it open. Without it, a responder we do not control - or a
// leftover instance of ourselves - makes the bind fail outright.
func reuseAddr(network, address string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return serr
}
