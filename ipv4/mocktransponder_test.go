// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build darwin freebsd linux netbsd openbsd

package ipv4_test

import (
	"code.google.com/p/go.net/ipv4"
	"net"
	"testing"
	"time"
)

// runPayloadTransponder transmits IPv4 datagram payloads to the
// loopback address or interface and captures the loopback'd datagram
// payloads.
func runPayloadTransponder(t *testing.T, c *ipv4.PacketConn, wb []byte, dst net.Addr) {
	cf := ipv4.FlagTTL | ipv4.FlagDst | ipv4.FlagInterface
	rb := make([]byte, 1500)
	for i, toggle := range []bool{true, false, true} {
		if err := c.SetControlMessage(cf, toggle); err != nil {
			t.Fatalf("ipv4.PacketConn.SetControlMessage failed: %v", err)
		}
		c.SetTOS(i + 1)
		var ip net.IP
		switch v := dst.(type) {
		case *net.UDPAddr:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip.IsMulticast() {
			c.SetMulticastTTL(i + 1)
		} else {
			c.SetTTL(i + 1)
		}
		c.SetDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err := c.Write(wb, nil, dst); err != nil {
			t.Fatalf("ipv4.PacketConn.Write failed: %v", err)
		}
		_, cm, _, err := c.Read(rb)
		if err != nil {
			t.Fatalf("ipv4.PacketConn.Read failed: %v", err)
		}
		t.Logf("rcvd cmsg: %v", cm)
	}
}

// runDatagramTransponder transmits ICMP for IPv4 datagrams to the
// loopback address or interface and captures the response datagrams
// from the protocol stack within the kernel.
func runDatagramTransponder(t *testing.T, c *ipv4.RawConn, wb []byte, src, dst net.Addr) {
	cf := ipv4.FlagTTL | ipv4.FlagDst | ipv4.FlagInterface
	rb := make([]byte, ipv4.HeaderLen+len(wb))
	for i, toggle := range []bool{true, false, true} {
		if err := c.SetControlMessage(cf, toggle); err != nil {
			t.Fatalf("ipv4.RawConn.SetControlMessage failed: %v", err)
		}
		wh := &ipv4.Header{}
		wh.Version = ipv4.Version
		wh.Len = ipv4.HeaderLen
		wh.TOS = i + 1
		wh.TotalLen = ipv4.HeaderLen + len(wb)
		wh.TTL = i + 1
		wh.Protocol = 1
		if src != nil {
			wh.Src = src.(*net.IPAddr).IP
		}
		if dst != nil {
			wh.Dst = dst.(*net.IPAddr).IP
		}
		c.SetDeadline(time.Now().Add(100 * time.Millisecond))
		if err := c.Write(wh, wb, nil); err != nil {
			t.Fatalf("ipv4.RawConn.Write failed: %v", err)
		}
		rh, _, cm, err := c.Read(rb)
		if err != nil {
			t.Fatalf("ipv4.RawConn.Read failed: %v", err)
		}
		t.Logf("rcvd cmsg: %v", cm.String())
		t.Logf("rcvd hdr: %v", rh.String())
	}
}

func loopbackInterface() *net.Interface {
	ift, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifi := range ift {
		if ifi.Flags&net.FlagLoopback != 0 {
			return &ifi
		}
	}
	return nil
}

func isGoodForMulticast(ifi *net.Interface) (net.IP, bool) {
	if ifi.Flags&net.FlagUp == 0 {
		return nil, false
	}
	// We need a unicast IPv4 address that can be used to specify
	// the IPv4 multicast interface.
	ifat, err := ifi.Addrs()
	if err != nil {
		return nil, false
	}
	if len(ifat) == 0 {
		return nil, false
	}
	var ip net.IP
	for _, ifa := range ifat {
		switch v := ifa.(type) {
		case *net.IPAddr:
			ip = v.IP
		case *net.IPNet:
			ip = v.IP
		default:
			continue
		}
		if ip.To4() == nil {
			ip = nil
			continue
		}
		break
	}
	if ip == nil {
		return nil, false
	}
	return ip, true
}
