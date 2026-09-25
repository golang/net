// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package quic

import (
	"crypto/tls"
	"testing"
	"testing/synctest"
)

func TestConnReceiveAckForUnsentPacket(t *testing.T) {
	synctest.Test(t, testConnReceiveAckForUnsentPacket)
}
func testConnReceiveAckForUnsentPacket(t *testing.T) {
	tc := newTestConn(t, serverSide, permissiveTransportParameters)
	tc.handshake()
	tc.writeFrames(packetType1RTT,
		debugFrameAck{
			ackDelay: 0,
			ranges:   []i64range[packetNumber]{{0, 10}},
		})
	tc.wantFrame("ACK for unsent packet causes CONNECTION_CLOSE",
		packetType1RTT, debugFrameConnectionCloseTransport{
			code: errProtocolViolation,
		})
}

// Issue #70703: If a packet contains both a CRYPTO frame which causes us to
// drop state for a number space, and also contains a valid ACK frame for that space,
// we shouldn't complain about the ACK.
func TestConnReceiveAckForDroppedSpace(t *testing.T) {
	synctest.Test(t, testConnReceiveAckForDroppedSpace)
}
func testConnReceiveAckForDroppedSpace(t *testing.T) {
	tc := newTestConn(t, serverSide, permissiveTransportParameters)
	tc.ignoreFrame(frameTypeAck)
	tc.ignoreFrame(frameTypeNewConnectionID)

	tc.writeFrames(packetTypeInitial,
		debugFrameCrypto{
			data: tc.cryptoDataIn[tls.QUICEncryptionLevelInitial],
		})
	tc.wantFrame("send Initial crypto",
		packetTypeInitial, debugFrameCrypto{
			data: tc.cryptoDataOut[tls.QUICEncryptionLevelInitial],
		})
	tc.wantFrame("send Handshake crypto",
		packetTypeHandshake, debugFrameCrypto{
			data: tc.cryptoDataOut[tls.QUICEncryptionLevelHandshake],
		})

	tc.writeFrames(packetTypeHandshake,
		debugFrameCrypto{
			data: tc.cryptoDataIn[tls.QUICEncryptionLevelHandshake],
		},
		debugFrameAck{
			ackDelay: 0,
			ranges:   []i64range[packetNumber]{{0, tc.lastPacket.num + 1}},
		})
	tc.wantFrame("handshake finishes",
		packetType1RTT, debugFrameHandshakeDone{})
	tc.wantIdle("connection is idle")
}

func TestConnReceiveInvalidFrameForPacket(t *testing.T) {
	synctestSubtest(t, "Initial", func(t *testing.T) {
		tc := newTestConn(t, clientSide, permissiveTransportParameters)
		tc.ignoreFrame(frameTypeAck)
		tc.ignoreFrame(frameTypeCrypto)
		tc.writeFrames(packetTypeInitial,
			debugFrameCrypto{
				data: tc.cryptoDataIn[tls.QUICEncryptionLevelInitial],
			},
			debugFrameStream{
				id:   0,
				off:  0,
				data: []byte{1},
			})
		tc.wantFrame("STREAM frame not allowed in Initial packet",
			packetTypeInitial, debugFrameConnectionCloseTransport{
				code: errProtocolViolation,
			})
	})
	synctestSubtest(t, "Handshake", func(t *testing.T) {
		tc := newTestConn(t, clientSide, permissiveTransportParameters)
		tc.ignoreFrame(frameTypeAck)
		tc.ignoreFrame(frameTypeCrypto)
		tc.writeFrames(packetTypeInitial,
			debugFrameCrypto{
				data: tc.cryptoDataIn[tls.QUICEncryptionLevelInitial],
			})
		tc.writeFrames(packetTypeHandshake,
			debugFrameCrypto{
				data: tc.cryptoDataIn[tls.QUICEncryptionLevelHandshake],
			},
			debugFrameStream{
				id:   0,
				off:  0,
				data: []byte{1},
			})
		// CONNECTION_CLOSE sent in both Initial and Handshake spaces.
		tc.wantFrame("STREAM frame not allowed in Handshake packet",
			packetTypeInitial, debugFrameConnectionCloseTransport{
				code: errProtocolViolation,
			})
		tc.wantFrame("STREAM frame not allowed in Handshake packet",
			packetTypeHandshake, debugFrameConnectionCloseTransport{
				code: errProtocolViolation,
			})
	})
	// There are currently no frames which may not be sent in a 1-RTT packet.
}
