// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipv4

import (
	"net"
	"reflect"
	"syscall"
)

func (c *genericOpt) sysfd() (syscall.Handle, error) {
	switch p := c.c.(type) {
	case *net.TCPConn, *net.UDPConn, *net.IPConn:
		return sysfd(p)
	}
	return syscall.InvalidHandle, errInvalidConnType
}

func (c *dgramOpt) sysfd() (syscall.Handle, error) {
	switch p := c.c.(type) {
	case *net.UDPConn, *net.IPConn:
		return sysfd(p.(net.Conn))
	}
	return syscall.InvalidHandle, errInvalidConnType
}

func (c *payloadHandler) sysfd() (syscall.Handle, error) {
	return sysfd(c.c.(net.Conn))
}

func (c *packetHandler) sysfd() (syscall.Handle, error) {
	return sysfd(c.c)
}

func sysfd(c net.Conn) (syscall.Handle, error) {
	cv := reflect.ValueOf(c)
	switch ce := cv.Elem(); ce.Kind() {
	case reflect.Struct:
		fd := ce.FieldByName("conn").FieldByName("fd")
		switch fe := fd.Elem(); fe.Kind() {
		case reflect.Struct:
			sysfd := fe.FieldByName("sysfd")
			return syscall.Handle(sysfd.Uint()), nil
		}
	}
	return syscall.InvalidHandle, errInvalidConnType
}
