// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"
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
		newConf := *conf
		conf = &newConf
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
	t                     *testing.T
	l                     *Listener
	now                   time.Time
	recvc                 chan *datagram
	idlec                 chan struct{}
	conns                 map[*Conn]*testConn
	acceptQueue           []*testConn
	configTransportParams []func(*transportParameters)
	configTestConn        []func(*testConn)
	sentDatagrams         [][]byte
	peerTLSConn           *tls.QUICConn
	lastInitialDstConnID  []byte // for parsing Retry packets
}

func newTestListener(t *testing.T, config *Config) *testListener {
	tl := &testListener{
		t:     t,
		now:   time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		recvc: make(chan *datagram),
		idlec: make(chan struct{}),
		conns: make(map[*Conn]*testConn),
	}
	var err error
	tl.l, err = newListener((*testListenerUDPConn)(tl), config, (*testListenerHooks)(tl))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tl.cleanup)
	return tl
}

func (tl *testListener) cleanup() {
	tl.l.Close(canceledContext())
}

func (tl *testListener) wait() {
	select {
	case tl.idlec <- struct{}{}:
	case <-tl.l.closec:
	}
	for _, tc := range tl.conns {
		tc.wait()
	}
}

// accept returns a server connection from the listener.
// Unlike Listener.Accept, connections are available as soon as they are created.
func (tl *testListener) accept() *testConn {
	if len(tl.acceptQueue) == 0 {
		tl.t.Fatalf("accept: expected available conn, but found none")
	}
	tc := tl.acceptQueue[0]
	tl.acceptQueue = tl.acceptQueue[1:]
	return tc
}

func (tl *testListener) write(d *datagram) {
	tl.recvc <- d
	tl.wait()
}

var testClientAddr = netip.MustParseAddrPort("10.0.0.1:8000")

func (tl *testListener) writeDatagram(d *testDatagram) {
	tl.t.Helper()
	logDatagram(tl.t, "<- listener under test receives", d)
	var buf []byte
	for _, p := range d.packets {
		tc := tl.connForDestination(p.dstConnID)
		if p.ptype != packetTypeRetry && tc != nil {
			space := spaceForPacketType(p.ptype)
			if p.num >= tc.peerNextPacketNum[space] {
				tc.peerNextPacketNum[space] = p.num + 1
			}
		}
		if p.ptype == packetTypeInitial {
			tl.lastInitialDstConnID = p.dstConnID
		}
		pad := 0
		if p.ptype == packetType1RTT {
			pad = d.paddedSize - len(buf)
		}
		buf = append(buf, encodeTestPacket(tl.t, tc, p, pad)...)
	}
	for len(buf) < d.paddedSize {
		buf = append(buf, 0)
	}
	addr := d.addr
	if !addr.IsValid() {
		addr = testClientAddr
	}
	tl.write(&datagram{
		b:    buf,
		addr: addr,
	})
}

func (tl *testListener) connForDestination(dstConnID []byte) *testConn {
	for _, tc := range tl.conns {
		for _, loc := range tc.conn.connIDState.local {
			if bytes.Equal(loc.cid, dstConnID) {
				return tc
			}
		}
	}
	return nil
}

func (tl *testListener) connForSource(srcConnID []byte) *testConn {
	for _, tc := range tl.conns {
		for _, loc := range tc.conn.connIDState.remote {
			if bytes.Equal(loc.cid, srcConnID) {
				return tc
			}
		}
	}
	return nil
}

func (tl *testListener) read() []byte {
	tl.t.Helper()
	tl.wait()
	if len(tl.sentDatagrams) == 0 {
		return nil
	}
	d := tl.sentDatagrams[0]
	tl.sentDatagrams = tl.sentDatagrams[1:]
	return d
}

func (tl *testListener) readDatagram() *testDatagram {
	tl.t.Helper()
	buf := tl.read()
	if buf == nil {
		return nil
	}
	p, _ := parseGenericLongHeaderPacket(buf)
	tc := tl.connForSource(p.dstConnID)
	d := parseTestDatagram(tl.t, tl, tc, buf)
	logDatagram(tl.t, "-> listener under test sends", d)
	return d
}

// wantDatagram indicates that we expect the Listener to send a datagram.
func (tl *testListener) wantDatagram(expectation string, want *testDatagram) {
	tl.t.Helper()
	got := tl.readDatagram()
	if !reflect.DeepEqual(got, want) {
		tl.t.Fatalf("%v:\ngot datagram:  %v\nwant datagram: %v", expectation, got, want)
	}
}

// wantIdle indicates that we expect the Listener to not send any more datagrams.
func (tl *testListener) wantIdle(expectation string) {
	if got := tl.readDatagram(); got != nil {
		tl.t.Fatalf("expect: %v\nunexpectedly got: %v", expectation, got)
	}
}

// advance causes time to pass.
func (tl *testListener) advance(d time.Duration) {
	tl.t.Helper()
	tl.advanceTo(tl.now.Add(d))
}

// advanceTo sets the current time.
func (tl *testListener) advanceTo(now time.Time) {
	tl.t.Helper()
	if tl.now.After(now) {
		tl.t.Fatalf("time moved backwards: %v -> %v", tl.now, now)
	}
	tl.now = now
	for _, tc := range tl.conns {
		if !tc.timer.After(tl.now) {
			tc.conn.sendMsg(timerEvent{})
			tc.wait()
		}
	}
}

// testListenerHooks implements listenerTestHooks.
type testListenerHooks testListener

func (tl *testListenerHooks) timeNow() time.Time {
	return tl.now
}

func (tl *testListenerHooks) newConn(c *Conn) {
	tc := newTestConnForConn(tl.t, (*testListener)(tl), c)
	tl.conns[c] = tc
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
