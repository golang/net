// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package socks

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"time"
)

var (
	noDeadline   = time.Time{}
	aLongTimeAgo = time.Unix(1, 0)
)

func (d *Dialer) connect(ctx context.Context, c net.Conn, req Request) (conn net.Conn, _ net.Addr, ctxErr error) {
	var udpHeader []byte

	host, port, err := splitHostPort(req.DstAddress)
	if err != nil {
		return c, nil, err
	}
	if deadline, ok := ctx.Deadline(); ok && !deadline.IsZero() {
		c.SetDeadline(deadline)
		if req.Cmd != CmdUDPAssociate {
			defer c.SetDeadline(noDeadline)
		}
	}
	if ctx != context.Background() {
		errCh := make(chan error, 1)
		done := make(chan struct{})
		defer func() {
			close(done)
			if ctxErr == nil {
				ctxErr = <-errCh
			}
		}()
		go func() {
			select {
			case <-ctx.Done():
				c.SetDeadline(aLongTimeAgo)
				errCh <- ctx.Err()
			case <-done:
				errCh <- nil
			}
		}()
	}

	conn = c
	b := make([]byte, 0, 6+len(host)) // the size here is just an estimate
	b = append(b, Version5)
	if len(d.AuthMethods) == 0 || d.Authenticate == nil {
		b = append(b, 1, byte(AuthMethodNotRequired))
	} else {
		ams := d.AuthMethods
		if len(ams) > 255 {
			return c, nil, errors.New("too many authentication methods")
		}
		b = append(b, byte(len(ams)))
		for _, am := range ams {
			b = append(b, byte(am))
		}
	}
	if _, ctxErr = c.Write(b); ctxErr != nil {
		return
	}

	if _, ctxErr = io.ReadFull(c, b[:2]); ctxErr != nil {
		return
	}
	if b[0] != Version5 {
		return c, nil, errors.New("unexpected protocol version " + strconv.Itoa(int(b[0])))
	}
	am := AuthMethod(b[1])
	if am == AuthMethodNoAcceptableMethods {
		return c, nil, errors.New("no acceptable authentication methods")
	}
	if d.Authenticate != nil {
		if ctxErr = d.Authenticate(ctx, c, am); ctxErr != nil {
			return
		}
	}

	b = b[:0]
	b = append(b, Version5, byte(req.Cmd), 0)
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			b = append(b, AddrTypeIPv4)
			b = append(b, ip4...)
		} else if ip6 := ip.To16(); ip6 != nil {
			b = append(b, AddrTypeIPv6)
			b = append(b, ip6...)
		} else {
			return c, nil, errors.New("unknown address type")
		}
	} else {
		if len(host) > 255 {
			return c, nil, errors.New("FQDN too long")
		}
		b = append(b, AddrTypeFQDN)
		b = append(b, byte(len(host)))
		b = append(b, host...)
	}
	b = append(b, byte(port>>8), byte(port))

	if req.Cmd == CmdUDPAssociate {
		udpHeader = make([]byte, len(b))
		copy(udpHeader[3:], b[3:])
	}

	if _, ctxErr = c.Write(b); ctxErr != nil {
		return
	}

	if _, ctxErr = io.ReadFull(c, b[:4]); ctxErr != nil {
		return
	}
	if b[0] != Version5 {
		return c, nil, errors.New("unexpected protocol version " + strconv.Itoa(int(b[0])))
	}
	if cmdErr := Reply(b[1]); cmdErr != StatusSucceeded {
		return c, nil, errors.New("unknown error " + cmdErr.String())
	}
	if b[2] != 0 {
		return c, nil, errors.New("non-zero reserved field")
	}
	l := 2
	addrType := b[3]
	var a Addr
	switch addrType {
	case AddrTypeIPv4:
		l += net.IPv4len
		a.IP = make(net.IP, net.IPv4len)
	case AddrTypeIPv6:
		l += net.IPv6len
		a.IP = make(net.IP, net.IPv6len)
	case AddrTypeFQDN:
		if _, err := io.ReadFull(c, b[:1]); err != nil {
			return c, nil, err
		}
		l += int(b[0])
	default:
		return c, nil, errors.New("unknown address type " + strconv.Itoa(int(b[3])))
	}

	if cap(b) < l {
		b = make([]byte, l)
	} else {
		b = b[:l]
	}
	if _, ctxErr = io.ReadFull(c, b); ctxErr != nil {
		return
	}
	if a.IP != nil {
		copy(a.IP, b)
	} else {
		a.Name = string(b[:len(b)-2])
	}
	a.Port = int(b[len(b)-2])<<8 | int(b[len(b)-1])

	if req.Cmd == CmdUDPAssociate {
		var uc net.Conn
		if uc, err = d.proxyDial(ctx, req.UDPNetwork, a.String()); err != nil {
			return c, &a, err
		}
		c.SetDeadline(noDeadline)
		go func() {
			defer uc.Close()
			io.Copy(io.Discard, c)
		}()
		return udpConn{Conn: uc, socksConn: c, header: udpHeader}, &a, nil
	}

	return c, &a, nil
}
