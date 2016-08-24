// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipv4

import (
	"net"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/net/internal/iana"
)

func getInt(s uintptr, opt *sockOpt) (int, error) {
	if opt.name < 1 || opt.typ != ssoTypeInt {
		return 0, errOpNoSupport
	}
	var i int32
	l := int32(4)
	if err := syscall.Getsockopt(syscall.Handle(s), iana.ProtocolIP, int32(opt.name), (*byte)(unsafe.Pointer(&i)), &l); err != nil {
		return 0, os.NewSyscallError("getsockopt", err)
	}
	return int(i), nil
}

func setInt(s uintptr, opt *sockOpt, v int) error {
	if opt.name < 1 || opt.typ != ssoTypeInt {
		return errOpNoSupport
	}
	i := int32(v)
	return os.NewSyscallError("setsockopt", syscall.Setsockopt(syscall.Handle(s), iana.ProtocolIP, int32(opt.name), (*byte)(unsafe.Pointer(&i)), 4))
}

func getInterface(s uintptr, opt *sockOpt) (*net.Interface, error) {
	if opt.name < 1 || opt.typ != ssoTypeInterface {
		return nil, errOpNoSupport
	}
	return getsockoptInterface(s, opt.name)
}

func setInterface(s uintptr, opt *sockOpt, ifi *net.Interface) error {
	if opt.name < 1 || opt.typ != ssoTypeInterface {
		return errOpNoSupport
	}
	return setsockoptInterface(s, opt.name, ifi)
}

func getICMPFilter(s uintptr, opt *sockOpt) (*ICMPFilter, error) {
	return nil, errOpNoSupport
}

func setICMPFilter(s uintptr, opt *sockOpt, f *ICMPFilter) error {
	return errOpNoSupport
}

func setGroup(s uintptr, opt *sockOpt, ifi *net.Interface, grp net.IP) error {
	if opt.name < 1 || opt.typ != ssoTypeIPMreq {
		return errOpNoSupport
	}
	return setsockoptIPMreq(s, opt.name, ifi, grp)
}

func setSourceGroup(s uintptr, opt *sockOpt, ifi *net.Interface, grp, src net.IP) error {
	// TODO(mikio): implement this
	return errOpNoSupport
}
