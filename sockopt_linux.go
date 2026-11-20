//go:build linux

package main

import (
	"net"
	"syscall"
	"unsafe"
)

const tcpQuickAck = 12 // TCP_QUICKACK

// setQuickAck enables TCP_QUICKACK on a connection: ACKs go out immediately
// instead of waiting for the delayed-ACK timer, which matters for the
// 2,000 sequential keep-alive reads.
func setQuickAck(conn *net.TCPConn) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		val := 1
		_, _, errno := syscall.Syscall6(syscall.SYS_SETSOCKOPT, fd,
			uintptr(syscall.IPPROTO_TCP), uintptr(tcpQuickAck),
			uintptr(unsafe.Pointer(&val)), uintptr(4), 0)
		_ = errno
	})
}
