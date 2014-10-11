// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build darwin dragonfly freebsd linux netbsd openbsd solaris windows

package ipv4_test

import (
	"os"
	"syscall"
)

func protocolNotSupported(err error) bool {
	switch err := err.(type) {
	case syscall.Errno:
		if err == syscall.EPROTONOSUPPORT {
			return true
		}
	case *os.SyscallError:
		switch err := err.Err.(type) {
		case syscall.Errno:
			if err == syscall.EPROTONOSUPPORT {
				return true
			}
		}
	}
	return false
}
