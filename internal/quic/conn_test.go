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
	ptype       packetType
	version     uint32
	num         packetNumber
	keyPhaseBit bool
	keyNumber   int
	dstConnID   []byte
	srcConnID   []byte
	frames      []debugFrame
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

// maxTestKeyPhases is the maximum number of 1-RTT keys we'll generate in a test.
const maxTestKeyPhases = 3

// A testConn is a Conn whose external interactions (sending and receiving packets,
// setting timers) can be manipulated in tests.
type testConn struct {
	t              *testing.T
	conn           *Conn
	listener       *testListener
	now            time.Time
	timer          time.Time
	timerLastFired time.Time
	idlec          chan struct{} // only accessed on the conn's loop

	// Keys are distinct from the conn's keys,
	// because the test may know about keys before the conn does.
	// For example, when sending a datagram with coalesced
	// Initial and Handshake packets to a client conn,
	// we use Handshake keys to encrypt the packet.
	// The client only acquires those keys when it processes
	// the Initial packet.
	keysInitial   fixedKeyPair
	keysHandshake fixedKeyPair
	rkeyAppData   test1RTTKeys
	wkeyAppData   test1RTTKeys
	rsecrets      [numberSpaceCount]keySecret
	wsecrets      [numberSpaceCount]keySecret

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
	sentDatagrams [][]byte
	sentPackets   []*testPacket
	sentFrames    []debugFrame
	lastPacket    *testPacket

	recvDatagram chan *datagram

	// Transport parameters sent by the conn.
	sentTransportParameters *transportParameters

	// Frame types to ignore in tests.
	ignoreFrames map[byte]bool

	// Values to set in packets sent to the conn.
	sendKeyNumber   int
	sendKeyPhaseBit bool

	asyncTestState
}

type test1RTTKeys struct {
	hdr headerKey
	pkt [maxTestKeyPhases]packetKey
}

type keySecret struct {
	suite  uint16
	secret []byte
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
		recvDatagram:  make(chan *datagram),
	}
	t.Cleanup(tc.cleanup)

	config := &Config{
		TLSConfig: newTestTLSConfig(side),
	}
	peerProvidedParams := defaultTransportParameters()
	peerProvidedParams.initialSrcConnID = testPeerConnID(0)
	if side == clientSide {
		peerProvidedParams.originalDstConnID = testLocalConnID(-1)
	}
	for _, o := range opts {
		switch o := o.(type) {
		case func(*Config):
			o(config)
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
		initialConnID = testPeerConnID(-1)
	}

	peerQUICConfig := &tls.QUICConfig{TLSConfig: newTestTLSConfig(side.peer())}
	if side == clientSide {
		tc.peerTLSConn = tls.QUICServer(peerQUICConfig)
	} else {
		tc.peerTLSConn = tls.QUICClient(peerQUICConfig)
	}
	tc.peerTLSConn.SetTransportParameters(marshalTransportParameters(peerProvidedParams))
	tc.peerTLSConn.Start(context.Background())

	tc.listener = newTestListener(t, config, (*testConnHooks)(tc))
	conn, err := tc.listener.l.newConn(
		tc.now,
		side,
		initialConnID,
		netip.MustParseAddrPort("127.0.0.1:443"))
	if err != nil {
		tc.t.Fatal(err)
	}
	tc.conn = conn

	conn.keysAppData.updateAfter = maxPacketNumber // disable key updates
	tc.keysInitial.r = conn.keysInitial.w
	tc.keysInitial.w = conn.keysInitial.r

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
		// We may have async ops that can proceed now that the conn is done.
		tc.wakeAsync()
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
	<-tc.conn.donec
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
		var s string
		switch p.ptype {
		case packetType1RTT:
			s = fmt.Sprintf("  %v pnum=%v", p.ptype, p.num)
		default:
			s = fmt.Sprintf("  %v pnum=%v ver=%v dst={%x} src={%x}", p.ptype, p.num, p.version, p.dstConnID, p.srcConnID)
		}
		if p.keyPhaseBit {
			s += fmt.Sprintf(" KeyPhase")
		}
		if p.keyNumber != 0 {
			s += fmt.Sprintf(" keynum=%v", p.keyNumber)
		}
		tc.t.Log(s)
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
	// TODO: This should use tc.listener.write.
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
			ptype:       ptype,
			num:         tc.peerNextPacketNum[space],
			keyNumber:   tc.sendKeyNumber,
			keyPhaseBit: tc.sendKeyPhaseBit,
			frames:      frames,
			version:     quicVersion1,
			dstConnID:   dstConnID,
			srcConnID:   tc.peerConnID,
		}},
	}
	if ptype == packetTypeInitial && tc.conn.side == serverSide {
		d.paddedSize = 1200
	}
	tc.write(d)
}

// writeAckForAll sends the Conn a datagram containing an ack for all packets up to the
// last one received.
func (tc *testConn) writeAckForAll() {
	tc.t.Helper()
	if tc.lastPacket == nil {
		return
	}
	tc.writeFrames(tc.lastPacket.ptype, debugFrameAck{
		ranges: []i64range[packetNumber]{{0, tc.lastPacket.num + 1}},
	})
}

// writeAckForLatest sends the Conn a datagram containing an ack for the
// most recent packet received.
func (tc *testConn) writeAckForLatest() {
	tc.t.Helper()
	if tc.lastPacket == nil {
		return
	}
	tc.writeFrames(tc.lastPacket.ptype, debugFrameAck{
		ranges: []i64range[packetNumber]{{tc.lastPacket.num, tc.lastPacket.num + 1}},
	})
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
	buf := tc.listener.read()
	if buf == nil {
		return nil
	}
	d := tc.parseTestDatagram(buf)
	// Log the datagram before removing ignored frames.
	// When things go wrong, it's useful to see all the frames.
	tc.logDatagram("-> conn under test sends", d)
	typeForFrame := func(f debugFrame) byte {
		// This is very clunky, and points at a problem
		// in how we specify what frames to ignore in tests.
		//
		// We mark frames to ignore using the frame type,
		// but we've got a debugFrame data structure here.
		// Perhaps we should be ignoring frames by debugFrame
		// type instead: tc.ignoreFrame[debugFrameAck]().
		switch f := f.(type) {
		case debugFramePadding:
			return frameTypePadding
		case debugFramePing:
			return frameTypePing
		case debugFrameAck:
			return frameTypeAck
		case debugFrameResetStream:
			return frameTypeResetStream
		case debugFrameStopSending:
			return frameTypeStopSending
		case debugFrameCrypto:
			return frameTypeCrypto
		case debugFrameNewToken:
			return frameTypeNewToken
		case debugFrameStream:
			return frameTypeStreamBase
		case debugFrameMaxData:
			return frameTypeMaxData
		case debugFrameMaxStreamData:
			return frameTypeMaxStreamData
		case debugFrameMaxStreams:
			if f.streamType == bidiStream {
				return frameTypeMaxStreamsBidi
			} else {
				return frameTypeMaxStreamsUni
			}
		case debugFrameDataBlocked:
			return frameTypeDataBlocked
		case debugFrameStreamDataBlocked:
			return frameTypeStreamDataBlocked
		case debugFrameStreamsBlocked:
			if f.streamType == bidiStream {
				return frameTypeStreamsBlockedBidi
			} else {
				return frameTypeStreamsBlockedUni
			}
		case debugFrameNewConnectionID:
			return frameTypeNewConnectionID
		case debugFrameRetireConnectionID:
			return frameTypeRetireConnectionID
		case debugFramePathChallenge:
			return frameTypePathChallenge
		case debugFramePathResponse:
			return frameTypePathResponse
		case debugFrameConnectionCloseTransport:
			return frameTypeConnectionCloseTransport
		case debugFrameConnectionCloseApplication:
			return frameTypeConnectionCloseApplication
		case debugFrameHandshakeDone:
			return frameTypeHandshakeDone
		}
		panic(fmt.Errorf("unhandled frame type %T", f))
	}
	for _, p := range d.packets {
		var frames []debugFrame
		for _, f := range p.frames {
			if !tc.ignoreFrames[typeForFrame(f)] {
				frames = append(frames, f)
			}
		}
		p.frames = frames
	}
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
	tc.lastPacket = p
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
		tc.sentFrames = p.frames
	}
	f := tc.sentFrames[0]
	tc.sentFrames = tc.sentFrames[1:]
	return f, tc.lastPacket.ptype
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

// wantFrameType indicates that we expect the Conn to send a frame,
// although we don't care about the contents.
func (tc *testConn) wantFrameType(expectation string, wantType packetType, want debugFrame) {
	tc.t.Helper()
	got, gotType := tc.readFrame()
	if got == nil {
		tc.t.Fatalf("%v:\nconnection is idle\nwant %v frame: %v", expectation, wantType, want)
	}
	if gotType != wantType {
		tc.t.Fatalf("%v:\ngot %v packet, want %v\ngot frame:  %v", expectation, gotType, wantType, got)
	}
	if reflect.TypeOf(got) != reflect.TypeOf(want) {
		tc.t.Fatalf("%v:\ngot frame:  %v\nwant frame of type: %v", expectation, got, want)
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
	w.appendPaddingTo(pad)
	if p.ptype != packetType1RTT {
		var k fixedKeys
		switch p.ptype {
		case packetTypeInitial:
			k = tc.keysInitial.w
		case packetTypeHandshake:
			k = tc.keysHandshake.w
		}
		if !k.isSet() {
			tc.t.Fatalf("sending %v packet with no write key", p.ptype)
		}
		w.finishProtectedLongHeaderPacket(pnumMaxAcked, k, longPacket{
			ptype:     p.ptype,
			version:   p.version,
			num:       p.num,
			dstConnID: p.dstConnID,
			srcConnID: p.srcConnID,
		})
	} else {
		if !tc.wkeyAppData.hdr.isSet() {
			tc.t.Fatalf("sending 1-RTT packet with no write key")
		}
		// Somewhat hackish: Generate a temporary updatingKeyPair that will
		// always use our desired key phase.
		k := &updatingKeyPair{
			w: updatingKeys{
				hdr: tc.wkeyAppData.hdr,
				pkt: [2]packetKey{
					tc.wkeyAppData.pkt[p.keyNumber],
					tc.wkeyAppData.pkt[p.keyNumber],
				},
			},
			updateAfter: maxPacketNumber,
		}
		if p.keyPhaseBit {
			k.phase |= keyPhaseBit
		}
		w.finish1RTTPacket(p.num, pnumMaxAcked, p.dstConnID, k)
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
		if isLongHeader(buf[0]) {
			var k fixedKeyPair
			switch ptype {
			case packetTypeInitial:
				k = tc.keysInitial
			case packetTypeHandshake:
				k = tc.keysHandshake
			}
			if !k.canRead() {
				tc.t.Fatalf("reading %v packet with no read key", ptype)
			}
			var pnumMax packetNumber // TODO: Track packet numbers.
			p, n := parseLongHeaderPacket(buf, k.r, pnumMax)
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
			if !tc.rkeyAppData.hdr.isSet() {
				tc.t.Fatalf("reading 1-RTT packet with no read key")
			}
			var pnumMax packetNumber // TODO: Track packet numbers.
			pnumOff := 1 + len(tc.peerConnID)
			// Try unprotecting the packet with the first maxTestKeyPhases keys.
			var phase int
			var pnum packetNumber
			var hdr []byte
			var pay []byte
			var err error
			for phase = 0; phase < maxTestKeyPhases; phase++ {
				b := append([]byte{}, buf...)
				hdr, pay, pnum, err = tc.rkeyAppData.hdr.unprotect(b, pnumOff, pnumMax)
				if err != nil {
					tc.t.Fatalf("1-RTT packet header parse error")
				}
				k := tc.rkeyAppData.pkt[phase]
				pay, err = k.unprotect(hdr, pay, pnum)
				if err == nil {
					break
				}
			}
			if err != nil {
				tc.t.Fatalf("1-RTT packet payload parse error")
			}
			frames, err := tc.parseTestFrames(pay)
			if err != nil {
				tc.t.Fatal(err)
			}
			d.packets = append(d.packets, &testPacket{
				ptype:       packetType1RTT,
				num:         pnum,
				dstConnID:   hdr[1:][:len(tc.peerConnID)],
				keyPhaseBit: hdr[0]&keyPhaseBit != 0,
				keyNumber:   phase,
				frames:      frames,
			})
			buf = buf[len(buf):]
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
		frames = append(frames, f)
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
	checkKey := func(typ string, secrets *[numberSpaceCount]keySecret, e tls.QUICEvent) {
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
		if secrets[space].secret == nil {
			secrets[space].suite = e.Suite
			secrets[space].secret = append([]byte{}, e.Data...)
		} else if secrets[space].suite != e.Suite || !bytes.Equal(secrets[space].secret, e.Data) {
			tc.t.Errorf("%v key mismatch for level for level %v", typ, e.Level)
		}
	}
	setAppDataKey := func(suite uint16, secret []byte, k *test1RTTKeys) {
		k.hdr.init(suite, secret)
		for i := 0; i < len(k.pkt); i++ {
			k.pkt[i].init(suite, secret)
			secret = updateSecret(suite, secret)
		}
	}
	switch e.Kind {
	case tls.QUICSetReadSecret:
		checkKey("write", &tc.wsecrets, e)
		switch e.Level {
		case tls.QUICEncryptionLevelHandshake:
			tc.keysHandshake.w.init(e.Suite, e.Data)
		case tls.QUICEncryptionLevelApplication:
			setAppDataKey(e.Suite, e.Data, &tc.wkeyAppData)
		}
	case tls.QUICSetWriteSecret:
		checkKey("read", &tc.rsecrets, e)
		switch e.Level {
		case tls.QUICEncryptionLevelHandshake:
			tc.keysHandshake.r.init(e.Suite, e.Data)
		case tls.QUICEncryptionLevelApplication:
			setAppDataKey(e.Suite, e.Data, &tc.rkeyAppData)
		}
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
			checkKey("write", &tc.rsecrets, e)
			switch e.Level {
			case tls.QUICEncryptionLevelHandshake:
				tc.keysHandshake.r.init(e.Suite, e.Data)
			case tls.QUICEncryptionLevelApplication:
				setAppDataKey(e.Suite, e.Data, &tc.rkeyAppData)
			}
		case tls.QUICSetWriteSecret:
			checkKey("read", &tc.wsecrets, e)
			switch e.Level {
			case tls.QUICEncryptionLevelHandshake:
				tc.keysHandshake.w.init(e.Suite, e.Data)
			case tls.QUICEncryptionLevelApplication:
				setAppDataKey(e.Suite, e.Data, &tc.wkeyAppData)
			}
		case tls.QUICWriteData:
			tc.cryptoDataIn[e.Level] = append(tc.cryptoDataIn[e.Level], e.Data...)
		case tls.QUICTransportParameters:
			p, err := unmarshalTransportParams(e.Data)
			if err != nil {
				tc.t.Logf("sent unparseable transport parameters %x %v", e.Data, err)
			} else {
				tc.sentTransportParameters = &p
			}
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

func (tc *testConnHooks) timeNow() time.Time {
	return tc.now
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
