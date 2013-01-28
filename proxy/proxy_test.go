// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package proxy

import (
	"io"
	"net"
	"net/url"
	"strconv"
	"sync"
	"testing"
)

type testFromURLDialer struct {
	network, addr string
}

func (t *testFromURLDialer) Dial(network, addr string) (net.Conn, error) {
	t.network = network
	t.addr = addr
	return nil, t
}

func (t *testFromURLDialer) Error() string {
	return "testFromURLDialer " + t.network + " " + t.addr
}

func TestFromURL(t *testing.T) {
	u, err := url.Parse("socks5://user:password@1.2.3.4:5678")
	if err != nil {
		t.Fatalf("failed to parse URL: %s", err)
	}

	tp := &testFromURLDialer{}
	proxy, err := FromURL(u, tp)
	if err != nil {
		t.Fatalf("FromURL failed: %s", err)
	}

	conn, err := proxy.Dial("tcp", "example.com:80")
	if conn != nil {
		t.Error("Dial unexpected didn't return an error")
	}
	if tp, ok := err.(*testFromURLDialer); ok {
		if tp.network != "tcp" || tp.addr != "1.2.3.4:5678" {
			t.Errorf("Dialer connected to wrong host. Wanted 1.2.3.4:5678, got: %v", tp)
		}
	} else {
		t.Errorf("Unexpected error from Dial: %s", err)
	}
}

func TestSOCKS5(t *testing.T) {
	endSystem, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer endSystem.Close()
	gateway, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer gateway.Close()

	wg := &sync.WaitGroup{}
	go socks5Gateway(t, gateway, endSystem, wg)
	wg.Add(1)

	proxy, err := SOCKS5("tcp", gateway.Addr().String(), nil, Direct)
	if err != nil {
		t.Fatalf("SOCKS5 failed: %v", err)
	}
	if c, err := proxy.Dial("tcp", endSystem.Addr().String()); err != nil {
		t.Fatalf("SOCKS5.Dial failed: %v", err)
	} else {
		c.Close()
	}

	wg.Wait()
}

func socks5Gateway(t *testing.T, gateway, endSystem net.Listener, wg *sync.WaitGroup) {
	defer wg.Done()

	c, err := gateway.Accept()
	if err != nil {
		t.Fatalf("net.Listener.Accept failed: %v", err)
	}
	defer c.Close()

	b := make([]byte, 32)
	if _, err := io.ReadFull(c, b[:3]); err != nil {
		t.Fatalf("net.Conn.Read failed: %v", err)
	}
	if _, err := c.Write([]byte{socks5Version, socks5AuthNone}); err != nil {
		t.Fatalf("net.Conn.Write failed: %v", err)
	}
	if _, err := io.ReadFull(c, b[:10]); err != nil {
		t.Fatalf("net.Conn.Read failed: %v", err)
	}
	if b[0] != socks5Version || b[1] != socks5Connect || b[2] != 0x00 || b[3] != socks5IP4 {
		t.Fatalf("got an unexpected packet: %v, %v, %v, %v", b[0], b[1], b[2], b[3])
	}
	copy(b[:4], []byte{socks5Version, 0x00, 0x00, socks5IP4})
	host, port, err := net.SplitHostPort(endSystem.Addr().String())
	if err != nil {
		t.Fatalf("net.SplitHostPort failed: %v", err)
	}
	b = append(b, []byte(net.ParseIP(host).To4())...)
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("strconv.Atoi failed: %v", err)
	}
	b = append(b, []byte{byte(p >> 8), byte(p)}...)
	if _, err := c.Write(b); err != nil {
		t.Fatalf("net.Conn.Write failed: %v", err)
	}
}
