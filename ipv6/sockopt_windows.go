// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipv6

import (
	"net"
	"os"
	"syscall"
	"unsafe"
)

func getInt(s uintptr, opt *sockOpt) (int, error) {
	if opt.name < 1 || opt.typ != ssoTypeInt {
		return 0, errOpNoSupport
	}
	var i int32
	l := int32(4)
	if err := syscall.Getsockopt(syscall.Handle(s), int32(opt.level), int32(opt.name), (*byte)(unsafe.Pointer(&i)), &l); err != nil {
		return 0, os.NewSyscallError("getsockopt", err)
	}
	return int(i), nil
}

func setInt(s uintptr, opt *sockOpt, v int) error {
	if opt.name < 1 || opt.typ != ssoTypeInt {
		return errOpNoSupport
	}
	i := int32(v)
	return os.NewSyscallError("setsockopt", syscall.Setsockopt(syscall.Handle(s), int32(opt.level), int32(opt.name), (*byte)(unsafe.Pointer(&i)), 4))
}

func getInterface(s uintptr, opt *sockOpt) (*net.Interface, error) {
	if opt.name < 1 || opt.typ != ssoTypeInterface {
		return nil, errOpNoSupport
	}
	var i int32
	l := int32(4)
	if err := syscall.Getsockopt(syscall.Handle(s), int32(opt.level), int32(opt.name), (*byte)(unsafe.Pointer(&i)), &l); err != nil {
		return nil, os.NewSyscallError("getsockopt", err)
	}
	if i == 0 {
		return nil, nil
	}
	ifi, err := net.InterfaceByIndex(int(i))
	if err != nil {
		return nil, err
	}
	return ifi, nil
}

func setInterface(s uintptr, opt *sockOpt, ifi *net.Interface) error {
	if opt.name < 1 || opt.typ != ssoTypeInterface {
		return errOpNoSupport
	}
	var i int32
	if ifi != nil {
		i = int32(ifi.Index)
	}
	return os.NewSyscallError("setsockopt", syscall.Setsockopt(syscall.Handle(s), int32(opt.level), int32(opt.name), (*byte)(unsafe.Pointer(&i)), 4))
}

func getICMPFilter(s uintptr, opt *sockOpt) (*ICMPFilter, error) {
	return nil, errOpNoSupport
}

func setICMPFilter(s uintptr, opt *sockOpt, f *ICMPFilter) error {
	return errOpNoSupport
}

func getMTUInfo(s uintptr, opt *sockOpt) (*net.Interface, int, error) {
	return nil, 0, errOpNoSupport
}

func setGroup(s uintptr, opt *sockOpt, ifi *net.Interface, grp net.IP) error {
	if opt.name < 1 || opt.typ != ssoTypeIPMreq {
		return errOpNoSupport
	}
	return setsockoptIPMreq(s, opt, ifi, grp)
}

func setSourceGroup(s uintptr, opt *sockOpt, ifi *net.Interface, grp, src net.IP) error {
	// TODO(mikio): implement this
	return errOpNoSupport
}
