// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build darwin freebsd linux netbsd openbsd

package ipv4_test

import (
	"code.google.com/p/go.net/ipv4"
	"net"
	"os"
	"testing"
)

func TestReadWriteUnicastIPPayloadUDP(t *testing.T) {
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.ListenPacket failed: %v", err)
	}
	defer c.Close()

	dst, err := net.ResolveUDPAddr("udp4", c.LocalAddr().String())
	if err != nil {
		t.Fatalf("net.ResolveUDPAddr failed: %v", err)
	}

	p := ipv4.NewPacketConn(c)
	runPayloadTransponder(t, p, []byte("HELLO-R-U-THERE"), dst)
}

func TestReadWriteUnicastIPPayloadICMP(t *testing.T) {
	if os.Getuid() != 0 {
		t.Logf("skipping test; must be root")
		return
	}

	c, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		t.Fatalf("net.ListenPacket failed: %v", err)
	}
	defer c.Close()

	dst, err := net.ResolveIPAddr("ip4", "127.0.0.1")
	if err != nil {
		t.Fatalf("ResolveIPAddr failed: %v", err)
	}

	p := ipv4.NewPacketConn(c)
	id := os.Getpid() & 0xffff
	pld := newICMPEchoRequest(id, 1, 128, []byte("HELLO-R-U-THERE"))
	runPayloadTransponder(t, p, pld, dst)
}

func TestReadWriteUnicastIPDatagram(t *testing.T) {
	if os.Getuid() != 0 {
		t.Logf("skipping test; must be root")
		return
	}

	c, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		t.Fatalf("net.ListenPacket failed: %v", err)
	}
	defer c.Close()

	dst, err := net.ResolveIPAddr("ip4", "127.0.0.1")
	if err != nil {
		t.Fatalf("ResolveIPAddr failed: %v", err)
	}

	r, err := ipv4.NewRawConn(c)
	if err != nil {
		t.Fatalf("ipv4.NewRawConn failed: %v", err)
	}
	id := os.Getpid() & 0xffff
	pld := newICMPEchoRequest(id, 1, 128, []byte("HELLO-R-U-THERE"))
	runDatagramTransponder(t, r, pld, nil, dst)
}
