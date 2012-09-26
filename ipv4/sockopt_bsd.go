// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build darwin freebsd netbsd openbsd

package ipv4

import (
	"net"
	"os"
	"syscall"
)

func ipv4MulticastTTL(fd int) (int, error) {
	v, err := syscall.GetsockoptByte(fd, syscall.IPPROTO_IP, syscall.IP_MULTICAST_TTL)
	if err != nil {
		return 0, os.NewSyscallError("getsockopt", err)
	}
	return int(v), nil
}

func setIPv4MulticastTTL(fd int, v int) error {
	err := syscall.SetsockoptByte(fd, syscall.IPPROTO_IP, syscall.IP_MULTICAST_TTL, byte(v))
	if err != nil {
		return os.NewSyscallError("setsockopt", err)
	}
	return nil
}

func ipv4ReceiveDestinationAddress(fd int) (bool, error) {
	v, err := syscall.GetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_RECVDSTADDR)
	if err != nil {
		return false, os.NewSyscallError("getsockopt", err)
	}
	return v == 1, nil
}

func setIPv4ReceiveDestinationAddress(fd int, v bool) error {
	err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_RECVDSTADDR, boolint(v))
	if err != nil {
		return os.NewSyscallError("setsockopt", err)
	}
	return nil
}

func ipv4ReceiveInterface(fd int) (bool, error) {
	v, err := syscall.GetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_RECVIF)
	if err != nil {
		return false, os.NewSyscallError("getsockopt", err)
	}
	return v == 1, nil
}

func setIPv4ReceiveInterface(fd int, v bool) error {
	err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_RECVIF, boolint(v))
	if err != nil {
		return os.NewSyscallError("setsockopt", err)
	}
	return nil
}

func ipv4MulticastInterface(fd int) (*net.Interface, error) {
	a, err := syscall.GetsockoptInet4Addr(fd, syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF)
	if err != nil {
		return nil, os.NewSyscallError("getsockopt", err)
	}
	return netIP4ToInterface(net.IPv4(a[0], a[1], a[2], a[3]))
}

func setIPv4MulticastInterface(fd int, ifi *net.Interface) error {
	ip, err := netInterfaceToIP4(ifi)
	if err != nil {
		return os.NewSyscallError("setsockopt", err)
	}
	var a [4]byte
	copy(a[:], ip.To4())
	err = syscall.SetsockoptInet4Addr(fd, syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF, a)
	if err != nil {
		return os.NewSyscallError("setsockopt", err)
	}
	return nil
}

func ipv4MulticastLoopback(fd int) (bool, error) {
	v, err := syscall.GetsockoptByte(fd, syscall.IPPROTO_IP, syscall.IP_MULTICAST_LOOP)
	if err != nil {
		return false, os.NewSyscallError("getsockopt", err)
	}
	return v == 1, nil
}

func setIPv4MulticastLoopback(fd int, v bool) error {
	err := syscall.SetsockoptByte(fd, syscall.IPPROTO_IP, syscall.IP_MULTICAST_LOOP, byte(boolint(v)))
	if err != nil {
		return os.NewSyscallError("setsockopt", err)
	}
	return nil
}

func joinIPv4Group(fd int, ifi *net.Interface, grp net.IP) error {
	mreq := &syscall.IPMreq{Multiaddr: [4]byte{grp[0], grp[1], grp[2], grp[3]}}
	if err := setSyscallIPMreq(mreq, ifi); err != nil {
		return err
	}
	err := syscall.SetsockoptIPMreq(fd, syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP, mreq)
	if err != nil {
		return os.NewSyscallError("setsockopt", err)
	}
	return nil
}

func leaveIPv4Group(fd int, ifi *net.Interface, grp net.IP) error {
	mreq := &syscall.IPMreq{Multiaddr: [4]byte{grp[0], grp[1], grp[2], grp[3]}}
	if err := setSyscallIPMreq(mreq, ifi); err != nil {
		return err
	}
	err := syscall.SetsockoptIPMreq(fd, syscall.IPPROTO_IP, syscall.IP_DROP_MEMBERSHIP, mreq)
	if err != nil {
		return os.NewSyscallError("setsockopt", err)
	}
	return nil
}
