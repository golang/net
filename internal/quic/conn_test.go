// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

var testVV = flag.Bool("vv", false, "even more verbose test output")

func TestConnTestConn(t *testing.T) {
	tc := newTestConn(t, serverSide)
	if got, want := tc.timeUntilEvent(), defaultMaxIdleTimeout; got != want {
		t.Errorf("new conn timeout=%v, want %v (max_idle_timeout)", got, want)
	}

	var ranAt time.Time
	tc.conn.runOnLoop(func(now time.Time, c *Conn) {
		ranAt = now
	})
	if !ranAt.Equal(tc.now) {
		t.Errorf("func ran on loop at %v, want %v", ranAt, tc.now)
	}
	tc.wait()

	nextTime := tc.now.Add(defaultMaxIdleTimeout / 2)
	tc.advanceTo(nextTime)
	tc.conn.runOnLoop(func(now time.Time, c *Conn) {
		ranAt = now
	})
	if !ranAt.Equal(nextTime) {
		t.Errorf("func ran on loop at %v, want %v", ranAt, nextTime)
	}
	tc.wait()

	tc.advanceToTimer()
	if !tc.conn.exited {
		t.Errorf("after advancing to idle timeout, exited = false, want true")
	}
}

type testDatagram struct {
	packets    []*testPacket
	paddedSize int
}

func (d testDatagram) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "datagram with %v packets", len(d.packets))
	if d.paddedSize > 0 {
		fmt.Fprintf(&b, " (padded to %v bytes)", d.paddedSize)
	}
	b.WriteString(":")
	for _, p := range d.packets {
		b.WriteString("\n")
		b.WriteString(p.String())
	}
	return b.String()
}

type testPacket struct {
	ptype     packetType
	version   uint32
	num       packetNumber
	dstConnID []byte
	srcConnID []byte
	frames    []debugFrame
}

func (p testPacket) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %v %v", p.ptype, p.num)
	if p.version != 0 {
		fmt.Fprintf(&b, " version=%v", p.version)
	}
	if p.srcConnID != nil {
		fmt.Fprintf(&b, " src={%x}", p.srcConnID)
	}
	if p.dstConnID != nil {
		fmt.Fprintf(&b, " dst={%x}", p.dstConnID)
	}
	for _, f := range p.frames {
		fmt.Fprintf(&b, "\n    %v", f)
	}
	return b.String()
}

// A testConn is a Conn whose external interactions (sending and receiving packets,
// setting timers) can be manipulated in tests.
type testConn struct {
	t              *testing.T
	conn           *Conn
	now            time.Time
	timer          time.Time
	timerLastFired time.Time
	idlec          chan struct{} // only accessed on the conn's loop

	// Read and write keys are distinct from the conn's keys,
	// because the test may know about keys before the conn does.
	// For example, when sending a datagram with coalesced
	// Initial and Handshake packets to a client conn,
	// we use Handshake keys to encrypt the packet.
	// The client only acquires those keys when it processes
	// the Initial packet.
	rkeys [numberSpaceCount]keyData // for packets sent to the conn
	wkeys [numberSpaceCount]keyData // for packets sent by the conn

	// testConn uses a test hook to snoop on the conn's TLS events.
	// CRYPTO data produced by the conn's QUICConn is placed in
	// cryptoDataOut.
	//
	// The peerTLSConn is is a QUICConn representing the peer.
	// CRYPTO data produced by the conn is written to peerTLSConn,
	// and data produced by peerTLSConn is placed in cryptoDataIn.
	cryptoDataOut map[tls.QUICEncryptionLevel][]byte
	cryptoDataIn  map[tls.QUICEncryptionLevel][]byte
	peerTLSConn   *tls.QUICConn

	// Information about the conn's (fake) peer.
	peerConnID        []byte                         // source conn id of peer's packets
	peerNextPacketNum [numberSpaceCount]packetNumber // next packet number to use

	// Datagrams, packets, and frames sent by the conn,
	// but not yet processed by the test.
	sentDatagrams   [][]byte
	sentPackets     []*testPacket
	sentFrames      []debugFrame
	sentFramePacket *testPacket

	// Frame types to ignore in tests.
	ignoreFrames map[byte]bool

	asyncTestState
}

type keyData struct {
	suite  uint16
	secret []byte
	k      keys
}

// newTestConn creates a Conn for testing.
//
// The Conn's event loop is controlled by the test,
// allowing test code to access Conn state directly
// by first ensuring the loop goroutine is idle.
func newTestConn(t *testing.T, side connSide, opts ...any) *testConn {
	t.Helper()
	tc := &testConn{
		t:          t,
		now:        time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		peerConnID: testPeerConnID(0),
		ignoreFrames: map[byte]bool{
			frameTypePadding: true, // ignore PADDING by default
		},
		cryptoDataOut: make(map[tls.QUICEncryptionLevel][]byte),
		cryptoDataIn:  make(map[tls.QUICEncryptionLevel][]byte),
	}
	t.Cleanup(tc.cleanup)

	config := &Config{
		TLSConfig: newTestTLSConfig(side),
	}
	peerProvidedParams := defaultTransportParameters()
	for _, o := range opts {
		switch o := o.(type) {
		case func(*tls.Config):
			o(config.TLSConfig)
		case func(p *transportParameters):
			o(&peerProvidedParams)
		default:
			t.Fatalf("unknown newTestConn option %T", o)
		}
	}

	var initialConnID []byte
	if side == serverSide {
		// The initial connection ID for the server is chosen by the client.
		// When creating a server-side connection, pick a random connection ID here.
		var err error
		initialConnID, err = newRandomConnID(0)
		if err != nil {
			tc.t.Fatal(err)
		}
	}

	peerQUICConfig := &tls.QUICConfig{TLSConfig: newTestTLSConfig(side.peer())}
	if side == clientSide {
		tc.peerTLSConn = tls.QUICServer(peerQUICConfig)
	} else {
		tc.peerTLSConn = tls.QUICClient(peerQUICConfig)
	}
	tc.peerTLSConn.SetTransportParameters(marshalTransportParameters(peerProvidedParams))
	tc.peerTLSConn.Start(context.Background())

	conn, err := newConn(
		tc.now,
		side,
		initialConnID,
		netip.MustParseAddrPort("127.0.0.1:443"),
		config,
		(*testConnListener)(tc),
		(*testConnHooks)(tc))
	if err != nil {
		tc.t.Fatal(err)
	}
	tc.conn = conn

	tc.wkeys[initialSpace].k = conn.wkeys[initialSpace]
	tc.rkeys[initialSpace].k = conn.rkeys[initialSpace]

	tc.wait()
	return tc
}

// advance causes time to pass.
func (tc *testConn) advance(d time.Duration) {
	tc.t.Helper()
	tc.advanceTo(tc.now.Add(d))
}

// advanceTo sets the current time.
func (tc *testConn) advanceTo(now time.Time) {
	tc.t.Helper()
	if tc.now.After(now) {
		tc.t.Fatalf("time moved backwards: %v -> %v", tc.now, now)
	}
	tc.now = now
	if tc.timer.After(tc.now) {
		return
	}
	tc.conn.sendMsg(timerEvent{})
	tc.wait()
}

// advanceToTimer sets the current time to the time of the Conn's next timer event.
func (tc *testConn) advanceToTimer() {
	if tc.timer.IsZero() {
		tc.t.Fatalf("advancing to timer, but timer is not set")
	}
	tc.advanceTo(tc.timer)
}

func (tc *testConn) timerDelay() time.Duration {
	if tc.timer.IsZero() {
		return math.MaxInt64 // infinite
	}
	if tc.timer.Before(tc.now) {
		return 0
	}
	return tc.timer.Sub(tc.now)
}

const infiniteDuration = time.Duration(math.MaxInt64)

// timeUntilEvent returns the amount of time until the next connection event.
func (tc *testConn) timeUntilEvent() time.Duration {
	if tc.timer.IsZero() {
		return infiniteDuration
	}
	if tc.timer.Before(tc.now) {
		return 0
	}
	return tc.timer.Sub(tc.now)
}

// wait blocks until the conn becomes idle.
// The conn is idle when it is blocked waiting for a packet to arrive or a timer to expire.
// Tests shouldn't need to call wait directly.
// testConn methods that wake the Conn event loop will call wait for them.
func (tc *testConn) wait() {
	tc.t.Helper()
	idlec := make(chan struct{})
	fail := false
	tc.conn.sendMsg(func(now time.Time, c *Conn) {
		if tc.idlec != nil {
			tc.t.Errorf("testConn.wait called concurrently")
			fail = true
			close(idlec)
		} else {
			// nextMessage will close idlec.
			tc.idlec = idlec
		}
	})
	select {
	case <-idlec:
	case <-tc.conn.donec:
	}
	if fail {
		panic(fail)
	}
}

func (tc *testConn) cleanup() {
	if tc.conn == nil {
		return
	}
	tc.conn.exit()
}

func (tc *testConn) logDatagram(text string, d *testDatagram) {
	tc.t.Helper()
	if !*testVV {
		return
	}
	pad := ""
	if d.paddedSize > 0 {
		pad = fmt.Sprintf(" (padded to %v)", d.paddedSize)
	}
	tc.t.Logf("%v datagram%v", text, pad)
	for _, p := range d.packets {
		switch p.ptype {
		case packetType1RTT:
			tc.t.Logf("  %v pnum=%v", p.ptype, p.num)
		default:
			tc.t.Logf("  %v pnum=%v ver=%v dst={%x} src={%x}", p.ptype, p.num, p.version, p.dstConnID, p.srcConnID)
		}
		for _, f := range p.frames {
			tc.t.Logf("    %v", f)
		}
	}
}

// write sends the Conn a datagram.
func (tc *testConn) write(d *testDatagram) {
	tc.t.Helper()
	var buf []byte
	tc.logDatagram("<- conn under test receives", d)
	for _, p := range d.packets {
		space := spaceForPacketType(p.ptype)
		if p.num >= tc.peerNextPacketNum[space] {
			tc.peerNextPacketNum[space] = p.num + 1
		}
		pad := 0
		if p.ptype == packetType1RTT {
			pad = d.paddedSize
		}
		buf = append(buf, tc.encodeTestPacket(p, pad)...)
	}
	for len(buf) < d.paddedSize {
		buf = append(buf, 0)
	}
	tc.conn.sendMsg(&datagram{
		b: buf,
	})
	tc.wait()
}

// writeFrame sends the Conn a datagram containing the given frames.
func (tc *testConn) writeFrames(ptype packetType, frames ...debugFrame) {
	tc.t.Helper()
	space := spaceForPacketType(ptype)
	dstConnID := tc.conn.connIDState.local[0].cid
	if tc.conn.connIDState.local[0].seq == -1 && ptype != packetTypeInitial {
		// Only use the transient connection ID in Initial packets.
		dstConnID = tc.conn.connIDState.local[1].cid
	}
	d := &testDatagram{
		packets: []*testPacket{{
			ptype:     ptype,
			num:       tc.peerNextPacketNum[space],
			frames:    frames,
			version:   1,
			dstConnID: dstConnID,
			srcConnID: tc.peerConnID,
		}},
	}
	if ptype == packetTypeInitial && tc.conn.side == serverSide {
		d.paddedSize = 1200
	}
	tc.write(d)
}

// ignoreFrame hides frames of the given type sent by the Conn.
func (tc *testConn) ignoreFrame(frameType byte) {
	tc.ignoreFrames[frameType] = true
}

// readDatagram reads the next datagram sent by the Conn.
// It returns nil if the Conn has no more datagrams to send at this time.
func (tc *testConn) readDatagram() *testDatagram {
	tc.t.Helper()
	tc.wait()
	tc.sentPackets = nil
	tc.sentFrames = nil
	if len(tc.sentDatagrams) == 0 {
		return nil
	}
	buf := tc.sentDatagrams[0]
	tc.sentDatagrams = tc.sentDatagrams[1:]
	d := tc.parseTestDatagram(buf)
	tc.logDatagram("-> conn under test sends", d)
	return d
}

// readPacket reads the next packet sent by the Conn.
// It returns nil if the Conn has no more packets to send at this time.
func (tc *testConn) readPacket() *testPacket {
	tc.t.Helper()
	for len(tc.sentPackets) == 0 {
		d := tc.readDatagram()
		if d == nil {
			return nil
		}
		tc.sentPackets = d.packets
	}
	p := tc.sentPackets[0]
	tc.sentPackets = tc.sentPackets[1:]
	return p
}

// readFrame reads the next frame sent by the Conn.
// It returns nil if the Conn has no more frames to send at this time.
func (tc *testConn) readFrame() (debugFrame, packetType) {
	tc.t.Helper()
	for len(tc.sentFrames) == 0 {
		p := tc.readPacket()
		if p == nil {
			return nil, packetTypeInvalid
		}
		tc.sentFramePacket = p
		tc.sentFrames = p.frames
	}
	f := tc.sentFrames[0]
	tc.sentFrames = tc.sentFrames[1:]
	return f, tc.sentFramePacket.ptype
}

// wantDatagram indicates that we expect the Conn to send a datagram.
func (tc *testConn) wantDatagram(expectation string, want *testDatagram) {
	tc.t.Helper()
	got := tc.readDatagram()
	if !reflect.DeepEqual(got, want) {
		tc.t.Fatalf("%v:\ngot datagram:  %v\nwant datagram: %v", expectation, got, want)
	}
}

// wantPacket indicates that we expect the Conn to send a packet.
func (tc *testConn) wantPacket(expectation string, want *testPacket) {
	tc.t.Helper()
	got := tc.readPacket()
	if !reflect.DeepEqual(got, want) {
		tc.t.Fatalf("%v:\ngot packet:  %v\nwant packet: %v", expectation, got, want)
	}
}

// wantFrame indicates that we expect the Conn to send a frame.
func (tc *testConn) wantFrame(expectation string, wantType packetType, want debugFrame) {
	tc.t.Helper()
	got, gotType := tc.readFrame()
	if got == nil {
		tc.t.Fatalf("%v:\nconnection is idle\nwant %v frame: %v", expectation, wantType, want)
	}
	if gotType != wantType {
		tc.t.Fatalf("%v:\ngot %v packet, want %v\ngot frame:  %v", expectation, gotType, wantType, got)
	}
	if !reflect.DeepEqual(got, want) {
		tc.t.Fatalf("%v:\ngot frame:  %v\nwant frame: %v", expectation, got, want)
	}
}

// wantIdle indicates that we expect the Conn to not send any more frames.
func (tc *testConn) wantIdle(expectation string) {
	tc.t.Helper()
	switch {
	case len(tc.sentFrames) > 0:
		tc.t.Fatalf("expect: %v\nunexpectedly got: %v", expectation, tc.sentFrames[0])
	case len(tc.sentPackets) > 0:
		tc.t.Fatalf("expect: %v\nunexpectedly got: %v", expectation, tc.sentPackets[0])
	}
	if f, _ := tc.readFrame(); f != nil {
		tc.t.Fatalf("expect: %v\nunexpectedly got: %v", expectation, f)
	}
}

func (tc *testConn) encodeTestPacket(p *testPacket, pad int) []byte {
	tc.t.Helper()
	var w packetWriter
	w.reset(1200)
	var pnumMaxAcked packetNumber
	if p.ptype != packetType1RTT {
		w.startProtectedLongHeaderPacket(pnumMaxAcked, longPacket{
			ptype:     p.ptype,
			version:   p.version,
			num:       p.num,
			dstConnID: p.dstConnID,
			srcConnID: p.srcConnID,
		})
	} else {
		w.start1RTTPacket(p.num, pnumMaxAcked, p.dstConnID)
	}
	for _, f := range p.frames {
		f.write(&w)
	}
	space := spaceForPacketType(p.ptype)
	if !tc.rkeys[space].k.isSet() {
		tc.t.Fatalf("sending packet with no %v keys available", space)
		return nil
	}
	w.appendPaddingTo(pad)
	if p.ptype != packetType1RTT {
		w.finishProtectedLongHeaderPacket(pnumMaxAcked, tc.rkeys[space].k, longPacket{
			ptype:     p.ptype,
			version:   p.version,
			num:       p.num,
			dstConnID: p.dstConnID,
			srcConnID: p.srcConnID,
		})
	} else {
		w.finish1RTTPacket(p.num, pnumMaxAcked, p.dstConnID, tc.rkeys[space].k)
	}
	return w.datagram()
}

func (tc *testConn) parseTestDatagram(buf []byte) *testDatagram {
	tc.t.Helper()
	bufSize := len(buf)
	d := &testDatagram{}
	size := len(buf)
	for len(buf) > 0 {
		if buf[0] == 0 {
			d.paddedSize = bufSize
			break
		}
		ptype := getPacketType(buf)
		space := spaceForPacketType(ptype)
		if !tc.wkeys[space].k.isSet() {
			tc.t.Fatalf("no keys for space %v, packet type %v", space, ptype)
		}
		if isLongHeader(buf[0]) {
			var pnumMax packetNumber // TODO: Track packet numbers.
			p, n := parseLongHeaderPacket(buf, tc.wkeys[space].k, pnumMax)
			if n < 0 {
				tc.t.Fatalf("packet parse error")
			}
			frames, err := tc.parseTestFrames(p.payload)
			if err != nil {
				tc.t.Fatal(err)
			}
			d.packets = append(d.packets, &testPacket{
				ptype:     p.ptype,
				version:   p.version,
				num:       p.num,
				dstConnID: p.dstConnID,
				srcConnID: p.srcConnID,
				frames:    frames,
			})
			buf = buf[n:]
		} else {
			var pnumMax packetNumber // TODO: Track packet numbers.
			p, n := parse1RTTPacket(buf, tc.wkeys[space].k, len(tc.peerConnID), pnumMax)
			if n < 0 {
				tc.t.Fatalf("packet parse error")
			}
			frames, err := tc.parseTestFrames(p.payload)
			if err != nil {
				tc.t.Fatal(err)
			}
			d.packets = append(d.packets, &testPacket{
				ptype:     packetType1RTT,
				num:       p.num,
				dstConnID: buf[1:][:len(tc.peerConnID)],
				frames:    frames,
			})
			buf = buf[n:]
		}
	}
	// This is rather hackish: If the last frame in the last packet
	// in the datagram is PADDING, then remove it and record
	// the padded size in the testDatagram.paddedSize.
	//
	// This makes it easier to write a test that expects a datagram
	// padded to 1200 bytes.
	if len(d.packets) > 0 && len(d.packets[len(d.packets)-1].frames) > 0 {
		p := d.packets[len(d.packets)-1]
		f := p.frames[len(p.frames)-1]
		if _, ok := f.(debugFramePadding); ok {
			p.frames = p.frames[:len(p.frames)-1]
			d.paddedSize = size
		}
	}
	return d
}

func (tc *testConn) parseTestFrames(payload []byte) ([]debugFrame, error) {
	tc.t.Helper()
	var frames []debugFrame
	for len(payload) > 0 {
		f, n := parseDebugFrame(payload)
		if n < 0 {
			return nil, errors.New("error parsing frames")
		}
		if !tc.ignoreFrames[payload[0]] {
			frames = append(frames, f)
		}
		payload = payload[n:]
	}
	return frames, nil
}

func spaceForPacketType(ptype packetType) numberSpace {
	switch ptype {
	case packetTypeInitial:
		return initialSpace
	case packetType0RTT:
		panic("TODO: packetType0RTT")
	case packetTypeHandshake:
		return handshakeSpace
	case packetTypeRetry:
		panic("TODO: packetTypeRetry")
	case packetType1RTT:
		return appDataSpace
	}
	panic("unknown packet type")
}

// testConnHooks implements connTestHooks.
type testConnHooks testConn

// handleTLSEvent processes TLS events generated by
// the connection under test's tls.QUICConn.
//
// We maintain a second tls.QUICConn representing the peer,
// and feed the TLS handshake data into it.
//
// We stash TLS handshake data from both sides in the testConn,
// where it can be used by tests.
//
// We snoop packet protection keys out of the tls.QUICConns,
// and verify that both sides of the connection are getting
// matching keys.
func (tc *testConnHooks) handleTLSEvent(e tls.QUICEvent) {
	setKey := func(keys *[numberSpaceCount]keyData, e tls.QUICEvent) {
		k, err := newKeys(e.Suite, e.Data)
		if err != nil {
			tc.t.Errorf("newKeys: %v", err)
			return
		}
		var space numberSpace
		switch {
		case e.Level == tls.QUICEncryptionLevelHandshake:
			space = handshakeSpace
		case e.Level == tls.QUICEncryptionLevelApplication:
			space = appDataSpace
		default:
			tc.t.Errorf("unexpected encryption level %v", e.Level)
			return
		}
		s := "read"
		if keys == &tc.wkeys {
			s = "write"
		}
		if keys[space].k.isSet() {
			if keys[space].suite != e.Suite || !bytes.Equal(keys[space].secret, e.Data) {
				tc.t.Errorf("%v key mismatch for level for level %v", s, e.Level)
			}
			return
		}
		keys[space].suite = e.Suite
		keys[space].secret = append([]byte{}, e.Data...)
		keys[space].k = k
	}
	switch e.Kind {
	case tls.QUICSetReadSecret:
		setKey(&tc.rkeys, e)
	case tls.QUICSetWriteSecret:
		setKey(&tc.wkeys, e)
	case tls.QUICWriteData:
		tc.cryptoDataOut[e.Level] = append(tc.cryptoDataOut[e.Level], e.Data...)
		tc.peerTLSConn.HandleData(e.Level, e.Data)
	}
	for {
		e := tc.peerTLSConn.NextEvent()
		switch e.Kind {
		case tls.QUICNoEvent:
			return
		case tls.QUICSetReadSecret:
			setKey(&tc.wkeys, e)
		case tls.QUICSetWriteSecret:
			setKey(&tc.rkeys, e)
		case tls.QUICWriteData:
			tc.cryptoDataIn[e.Level] = append(tc.cryptoDataIn[e.Level], e.Data...)
		}
	}
}

// nextMessage is called by the Conn's event loop to request its next event.
func (tc *testConnHooks) nextMessage(msgc chan any, timer time.Time) (now time.Time, m any) {
	tc.timer = timer
	for {
		if !timer.IsZero() && !timer.After(tc.now) {
			if timer.Equal(tc.timerLastFired) {
				// If the connection timer fires at time T, the Conn should take some
				// action to advance the timer into the future. If the Conn reschedules
				// the timer for the same time, it isn't making progress and we have a bug.
				tc.t.Errorf("connection timer spinning; now=%v timer=%v", tc.now, timer)
			} else {
				tc.timerLastFired = timer
				return tc.now, timerEvent{}
			}
		}
		select {
		case m := <-msgc:
			return tc.now, m
		default:
		}
		if !tc.wakeAsync() {
			break
		}
	}
	// If the message queue is empty, then the conn is idle.
	if tc.idlec != nil {
		idlec := tc.idlec
		tc.idlec = nil
		close(idlec)
	}
	m = <-msgc
	return tc.now, m
}

func (tc *testConnHooks) newConnID(seq int64) ([]byte, error) {
	return testLocalConnID(seq), nil
}

// testLocalConnID returns the connection ID with a given sequence number
// used by a Conn under test.
func testLocalConnID(seq int64) []byte {
	cid := make([]byte, connIDLen)
	copy(cid, []byte{0xc0, 0xff, 0xee})
	cid[len(cid)-1] = byte(seq)
	return cid
}

// testPeerConnID returns the connection ID with a given sequence number
// used by the fake peer of a Conn under test.
func testPeerConnID(seq int64) []byte {
	// Use a different length than we choose for our own conn ids,
	// to help catch any bad assumptions.
	return []byte{0xbe, 0xee, 0xff, byte(seq)}
}

// testConnListener implements connListener.
type testConnListener testConn

func (tc *testConnListener) sendDatagram(p []byte, addr netip.AddrPort) error {
	tc.sentDatagrams = append(tc.sentDatagrams, append([]byte(nil), p...))
	return nil
}

// canceledContext returns a canceled Context.
//
// Functions which take a context preference progress over cancelation.
// For example, a read with a canceled context will return data if any is available.
// Tests use canceled contexts to perform non-blocking operations.
func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
