// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package socks

import (
	"errors"
	"net"
	"strconv"
)

var (
	errHeaderTooSmall     = errors.New("packet header is too small")
	errFragNotImplemented = errors.New("packet fragmentation is not implemented")
)

type udpConn struct {
	net.Conn
	socksConn net.Conn
	header    []byte
}

func (udp udpConn) Close() error {
	defer udp.socksConn.Close()
	return udp.Conn.Close()
}

func (udp udpConn) Read(b []byte) (int, error) {
	buf := make([]byte, 262+len(b))
	n, err := udp.Conn.Read(buf)
	buf, hdrErr := SkipUDPHeader(buf[:n])
	if hdrErr != nil {
		if err == nil {
			err = hdrErr
		}
		return 0, err
	}
	n = copy(b, buf)
	return n, err
}

func (udp udpConn) Write(b []byte) (int, error) {
	buf := make([]byte, len(udp.header)+len(b))
	n := copy(buf, udp.header)
	copy(buf[n:], b)
	n, err := udp.Conn.Write(buf)
	if n >= len(udp.header) {
		n -= len(udp.header)
	} else {
		n = 0
	}
	return n, err
}

func SkipUDPHeader(buf []byte) ([]byte, error) {
	if len(buf) < 4 {
		return nil, errHeaderTooSmall
	}
	frag := buf[2]
	addrType := buf[3]
	buf = buf[4:]
	switch addrType {
	case AddrTypeIPv4:
		if len(buf) < net.IPv4len+2 {
			return nil, errHeaderTooSmall
		}
		buf = buf[net.IPv4len+2:]
	case AddrTypeIPv6:
		if len(buf) < net.IPv6len+2 {
			return nil, errHeaderTooSmall
		}
		buf = buf[net.IPv6len+2:]
	case AddrTypeFQDN:
		if len(buf) == 0 || len(buf) < 1+int(buf[0])+2 {
			return nil, errHeaderTooSmall
		}
		buf = buf[1+int(buf[0])+2:]
	default:
		return nil, errors.New("unknown address type " + strconv.Itoa(int(addrType)))
	}
	if frag != 0 {
		return nil, errFragNotImplemented
	}
	return buf, nil
}
