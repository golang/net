// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"crypto/tls"
	"testing"
)

// Frames may be retransmitted either when the packet containing the frame is lost, or on PTO.
// lostFrameTest runs a test in both configurations.
func lostFrameTest(t *testing.T, f func(t *testing.T, pto bool)) {
	t.Run("lost", func(t *testing.T) {
		f(t, false)
	})
	t.Run("pto", func(t *testing.T) {
		f(t, true)
	})
}

// triggerLossOrPTO causes the conn to declare the last sent packet lost,
// or advances to the PTO timer.
func (tc *testConn) triggerLossOrPTO(ptype packetType, pto bool) {
	tc.t.Helper()
	if pto {
		if !tc.conn.loss.ptoTimerArmed {
			tc.t.Fatalf("PTO timer not armed, expected it to be")
		}
		tc.advanceTo(tc.conn.loss.timer)
		return
	}
	defer func(ignoreFrames map[byte]bool) {
		tc.ignoreFrames = ignoreFrames
	}(tc.ignoreFrames)
	tc.ignoreFrames = map[byte]bool{
		frameTypeAck:     true,
		frameTypePadding: true,
	}
	// Send three packets containing PINGs, and then respond with an ACK for the
	// last one. This puts the last packet before the PINGs outside the packet
	// reordering threshold, and it will be declared lost.
	const lossThreshold = 3
	var num packetNumber
	for i := 0; i < lossThreshold; i++ {
		tc.conn.ping(spaceForPacketType(ptype))
		d := tc.readDatagram()
		if d == nil {
			tc.t.Fatalf("conn is idle; want PING frame")
		}
		if d.packets[0].ptype != ptype {
			tc.t.Fatalf("conn sent %v packet; want %v", d.packets[0].ptype, ptype)
		}
		num = d.packets[0].num
	}
	tc.writeFrames(ptype, debugFrameAck{
		ranges: []i64range[packetNumber]{
			{num, num + 1},
		},
	})
}

func TestLostCRYPTOFrame(t *testing.T) {
	// "Data sent in CRYPTO frames is retransmitted [...] until all data has been acknowledged."
	// https://www.rfc-editor.org/rfc/rfc9000.html#section-13.3-3.1
	lostFrameTest(t, func(t *testing.T, pto bool) {
		tc := newTestConn(t, clientSide)
		tc.ignoreFrame(frameTypeAck)

		tc.wantFrame("client sends Initial CRYPTO frame",
			packetTypeInitial, debugFrameCrypto{
				data: tc.cryptoDataOut[tls.QUICEncryptionLevelInitial],
			})
		tc.triggerLossOrPTO(packetTypeInitial, pto)
		tc.wantFrame("client resends Initial CRYPTO frame",
			packetTypeInitial, debugFrameCrypto{
				data: tc.cryptoDataOut[tls.QUICEncryptionLevelInitial],
			})

		tc.writeFrames(packetTypeInitial,
			debugFrameCrypto{
				data: tc.cryptoDataIn[tls.QUICEncryptionLevelInitial],
			})
		tc.writeFrames(packetTypeHandshake,
			debugFrameCrypto{
				data: tc.cryptoDataIn[tls.QUICEncryptionLevelHandshake],
			})

		tc.wantFrame("client sends Handshake CRYPTO frame",
			packetTypeHandshake, debugFrameCrypto{
				data: tc.cryptoDataOut[tls.QUICEncryptionLevelHandshake],
			})
		tc.triggerLossOrPTO(packetTypeHandshake, pto)
		tc.wantFrame("client resends Handshake CRYPTO frame",
			packetTypeHandshake, debugFrameCrypto{
				data: tc.cryptoDataOut[tls.QUICEncryptionLevelHandshake],
			})
	})
}

func TestLostHandshakeDoneFrame(t *testing.T) {
	// "The HANDSHAKE_DONE frame MUST be retransmitted until it is acknowledged."
	// https://www.rfc-editor.org/rfc/rfc9000.html#section-13.3-3.16
	lostFrameTest(t, func(t *testing.T, pto bool) {
		tc := newTestConn(t, serverSide)
		tc.ignoreFrame(frameTypeAck)

		tc.writeFrames(packetTypeInitial,
			debugFrameCrypto{
				data: tc.cryptoDataIn[tls.QUICEncryptionLevelInitial],
			})
		tc.wantFrame("server sends Initial CRYPTO frame",
			packetTypeInitial, debugFrameCrypto{
				data: tc.cryptoDataOut[tls.QUICEncryptionLevelInitial],
			})
		tc.wantFrame("server sends Handshake CRYPTO frame",
			packetTypeHandshake, debugFrameCrypto{
				data: tc.cryptoDataOut[tls.QUICEncryptionLevelHandshake],
			})
		tc.writeFrames(packetTypeHandshake,
			debugFrameCrypto{
				data: tc.cryptoDataIn[tls.QUICEncryptionLevelHandshake],
			})

		tc.wantFrame("server sends HANDSHAKE_DONE after handshake completes",
			packetType1RTT, debugFrameHandshakeDone{})
		tc.wantFrame("server sends session ticket in CRYPTO frame",
			packetType1RTT, debugFrameCrypto{
				data: tc.cryptoDataOut[tls.QUICEncryptionLevelApplication],
			})

		tc.triggerLossOrPTO(packetType1RTT, pto)
		tc.wantFrame("server resends HANDSHAKE_DONE",
			packetType1RTT, debugFrameHandshakeDone{})
		tc.wantFrame("server resends session ticket",
			packetType1RTT, debugFrameCrypto{
				data: tc.cryptoDataOut[tls.QUICEncryptionLevelApplication],
			})
	})
}
