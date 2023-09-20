// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
)

func TestConnect(t *testing.T) {
	newLocalConnPair(t, &Config{}, &Config{})
}

func TestStreamTransfer(t *testing.T) {
	ctx := context.Background()
	cli, srv := newLocalConnPair(t, &Config{}, &Config{})
	data := makeTestData(1 << 20)

	srvdone := make(chan struct{})
	go func() {
		defer close(srvdone)
		s, err := srv.AcceptStream(ctx)
		if err != nil {
			t.Errorf("AcceptStream: %v", err)
			return
		}
		b, err := io.ReadAll(s)
		if err != nil {
			t.Errorf("io.ReadAll(s): %v", err)
			return
		}
		if !bytes.Equal(b, data) {
			t.Errorf("read data mismatch (got %v bytes, want %v", len(b), len(data))
		}
		if err := s.Close(); err != nil {
			t.Errorf("s.Close() = %v", err)
		}
	}()

	s, err := cli.NewStream(ctx)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	n, err := io.Copy(s, bytes.NewBuffer(data))
	if n != int64(len(data)) || err != nil {
		t.Fatalf("io.Copy(s, data) = %v, %v; want %v, nil", n, err, len(data))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("s.Close() = %v", err)
	}
}

func newLocalConnPair(t *testing.T, conf1, conf2 *Config) (clientConn, serverConn *Conn) {
	t.Helper()
	ctx := context.Background()
	l1 := newLocalListener(t, serverSide, conf1)
	l2 := newLocalListener(t, clientSide, conf2)
	c2, err := l2.Dial(ctx, "udp", l1.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	c1, err := l1.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return c2, c1
}

func newLocalListener(t *testing.T, side connSide, conf *Config) *Listener {
	t.Helper()
	if conf.TLSConfig == nil {
		conf.TLSConfig = newTestTLSConfig(side)
	}
	l, err := Listen("udp", "127.0.0.1:0", conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		l.Close(context.Background())
	})
	return l
}

type testListener struct {
	t             *testing.T
	l             *Listener
	recvc         chan *datagram
	idlec         chan struct{}
	sentDatagrams [][]byte
}

func newTestListener(t *testing.T, config *Config, testHooks connTestHooks) *testListener {
	tl := &testListener{
		t:     t,
		recvc: make(chan *datagram),
		idlec: make(chan struct{}),
	}
	tl.l = newListener((*testListenerUDPConn)(tl), config, testHooks)
	t.Cleanup(tl.cleanup)
	return tl
}

func (tl *testListener) cleanup() {
	tl.l.Close(canceledContext())
}

func (tl *testListener) wait() {
	tl.idlec <- struct{}{}
}

func (tl *testListener) write(d *datagram) {
	tl.recvc <- d
	tl.wait()
}

func (tl *testListener) read() []byte {
	tl.wait()
	if len(tl.sentDatagrams) == 0 {
		return nil
	}
	d := tl.sentDatagrams[0]
	tl.sentDatagrams = tl.sentDatagrams[1:]
	return d
}

// testListenerUDPConn implements UDPConn.
type testListenerUDPConn testListener

func (tl *testListenerUDPConn) Close() error {
	close(tl.recvc)
	return nil
}

func (tl *testListenerUDPConn) LocalAddr() net.Addr {
	return net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:443"))
}

func (tl *testListenerUDPConn) ReadMsgUDPAddrPort(b, control []byte) (n, controln, flags int, _ netip.AddrPort, _ error) {
	for {
		select {
		case d, ok := <-tl.recvc:
			if !ok {
				return 0, 0, 0, netip.AddrPort{}, io.EOF
			}
			n = copy(b, d.b)
			return n, 0, 0, d.addr, nil
		case <-tl.idlec:
		}
	}
}

func (tl *testListenerUDPConn) WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error) {
	tl.sentDatagrams = append(tl.sentDatagrams, append([]byte(nil), b...))
	return len(b), nil
}
