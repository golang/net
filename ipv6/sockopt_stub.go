// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build nacl plan9 solaris

package ipv6

import "net"

func getInt(fd int, opt *sockOpt) (int, error) {
	return 0, errOpNoSupport
}

func setInt(fd int, opt *sockOpt, v int) error {
	return errOpNoSupport
}

func getInterface(fd int, opt *sockOpt) (*net.Interface, error) {
	return nil, errOpNoSupport
}

func setInterface(fd int, opt *sockOpt, ifi *net.Interface) error {
	return errOpNoSupport
}

func getICMPFilter(fd int, opt *sockOpt) (*ICMPFilter, error) {
	return nil, errOpNoSupport
}

func setICMPFilter(fd int, opt *sockOpt, f *ICMPFilter) error {
	return errOpNoSupport
}

func getMTUInfo(fd int, opt *sockOpt) (*net.Interface, int, error) {
	return nil, 0, errOpNoSupport
}

func setGroup(fd int, opt *sockOpt, ifi *net.Interface, grp net.IP) error {
	return errOpNoSupport
}
