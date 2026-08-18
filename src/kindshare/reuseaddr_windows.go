//go:build windows

package main

import "syscall"

// reuseAddr mirrors the unix build so the daemon still compiles on the machine
// it is cross-compiled from.
func reuseAddr(network, address string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return serr
}
