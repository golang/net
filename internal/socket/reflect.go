// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build !go1.9

package socket

import (
	"net"
	"os"

	"golang.org/x/net/internal/netreflect"
)

// A Conn represents a raw connection.
type Conn struct {
	c net.Conn
}

// NewConn returns a new raw connection.
func NewConn(c net.Conn) (*Conn, error) {
	return &Conn{c: c}, nil
}

func (o *Option) get(c *Conn, b []byte) (int, error) {
	s, err := netreflect.SocketOf(c.c)
	if err != nil {
		return 0, err
	}
	n, err := getsockopt(s, o.Level, o.Name, b)
	return n, os.NewSyscallError("getsockopt", err)
}

func (o *Option) set(c *Conn, b []byte) error {
	s, err := netreflect.SocketOf(c.c)
	if err != nil {
		return err
	}
	return os.NewSyscallError("setsockopt", setsockopt(s, o.Level, o.Name, b))
}
