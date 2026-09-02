// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package quic

import (
	"crypto/tls"
	"testing"
	"testing/synctest"
	"time"
)

func TestAckElicitingAck(t *testing.T) {
	synctest.Test(t, testAckElicitingAck)
}
func testAckElicitingAck(t *testing.T) {
	// "A receiver that sends only non-ack-eliciting packets [...] might not receive
	// an acknowledgment for a long period of time.
	// [...] a receiver could send a [...] ack-eliciting frame occasionally [...]
	// to elicit an ACK from the peer."
	// https://www.rfc-editor.org/rfc/rfc9000#section-13.2.4-2
	//
	// Send a bunch of ack-eliciting packets, verify that the conn doesn't just
	// send ACKs in response.
	tc := newTestConn(t, clientSide, permissiveTransportParameters)
	tc.handshake()
	const count = 100
	for i := 0; i < count; i++ {
		time.Sleep(1 * time.Millisecond)
		tc.writeFrames(packetType1RTT,
			debugFramePing{},
		)
		got, _ := tc.readFrame()
		switch got.(type) {
		case debugFrameAck:
			continue
		case debugFramePing:
			return
		}
	}
	t.Errorf("after sending %v PINGs, got no ack-eliciting response", count)
}

func TestSendPacketNumberSize(t *testing.T) {
	synctest.Test(t, testSendPacketNumberSize)
}
func testSendPacketNumberSize(t *testing.T) {
	tc := newTestConn(t, clientSide, permissiveTransportParameters)
	tc.handshake()

	recvPing := func() *testPacket {
		t.Helper()
		tc.conn.ping(appDataSpace)
		p := tc.readPacket()
		if p == nil {
			t.Fatalf("want packet containing PING, got none")
		}
		return p
	}

	// Desynchronize the packet numbers the conn is sending and the ones it is receiving,
	// by having the conn send a number of unacked packets.
	for i := 0; i < 16; i++ {
		recvPing()
	}

	// Establish the maximum packet number the conn has received an ACK for.
	maxAcked := recvPing().num
	tc.writeAckForAll()

	// Make the conn send a sequence of packets.
	// Check that the packet number is encoded with two bytes once the difference between the
	// current packet and the max acked one is sufficiently large.
	for want := maxAcked + 1; want < maxAcked+0x100; want++ {
		p := recvPing()
		if p.num == want+1 {
			// The conn skipped a packet number
			// (defense against optimistic ACK attacks).
			want++
		} else if p.num != want {
			t.Fatalf("received packet number %v, want %v", p.num, want)
		}
		gotPnumLen := int(p.header&0x03) + 1
		wantPnumLen := 1
		if p.num-maxAcked >= 0x80 {
			wantPnumLen = 2
		}
		if gotPnumLen != wantPnumLen {
			t.Fatalf("packet number 0x%x encoded with %v bytes, want %v (max acked = %v)", p.num, gotPnumLen, wantPnumLen, maxAcked)
		}
	}
}

func TestConnSendAntiAmplificationInitialFlightBlocked(t *testing.T) {
	synctest.Test(t, testConnSendAntiAmplificationInitialFlightBlocked)
}
func testConnSendAntiAmplificationInitialFlightBlocked(t *testing.T) {
	tc := newTestConn(t, serverSide, permissiveTransportParameters, func(c *Config) {
		c.TLSConfig.Certificates = []tls.Certificate{bigCert}
	})

	// Client sends an Initial packet, datagram padded to 1200 bytes.
	tc.writeFrames(packetTypeInitial,
		debugFrameCrypto{
			data: tc.cryptoDataIn[tls.QUICEncryptionLevelInitial],
		})
	bytesSent := 1200

	// Server sends as much data as it can.
	// Each packet is padded to 1200 bytes.
	// Server is blocked by the anti-amplification limit.
	bytesRead := 0
	for {
		dgram := tc.endpoint.read()
		if dgram == nil {
			break
		}
		bytesRead += len(dgram)
	}
	if got, want := bytesRead, 3*bytesSent; got != want {
		t.Fatalf("server sent %v bytes in its initial flight; want %v", got, want)
	}

	// Wait until the server's PTO timer fires.
	// The server can't send a PTO probe, however,
	// because it's still blocked by the anti-amplification limit.
	time.Sleep(2 * time.Second)

	// Client sends a small packet, increasing the anti-amplification limit
	// but not by enough to permit the server to send another fully-padded packet.
	//
	// The following is a hand-crafted 0-RTT-ish packet.
	// It isn't valid, but it has the right DCID so it gets routed to our conn
	// and increases the anti-amplification limit.
	dcid := tc.conn.connIDState.local[0].cid
	pkt := []byte{
		0b11010000,
		0, 0, 0, 1,
		byte(len(dcid)),
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1,
	}
	copy(pkt[6:], dcid)
	tc.endpoint.write(&datagram{
		b:        pkt,
		peerAddr: tc.conn.peerAddr,
	})
	bytesSent += len(pkt)

	// We've just increased the server's anti-amplification limit.
	// Ensure it doesn't exceed the limit with whatever it sends now.
	for {
		dgram := tc.endpoint.read()
		if dgram == nil {
			break
		}
		bytesRead += len(dgram)
	}
	if bytesRead > 3*bytesSent {
		t.Fatalf("server exceeded anti-amplification limit: sent %v bytes > 3*%v bytes received", bytesRead, bytesSent)
	}
}
