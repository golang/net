// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import "testing"

func TestPing(t *testing.T) {
	tc := newTestConn(t, clientSide)
	tc.conn.ping(initialSpace)
	tc.wantFrame("connection should send a PING frame",
		packetTypeInitial, debugFramePing{})

	tc.advanceToTimer()
	tc.wantFrame("on PTO, connection should send another PING frame",
		packetTypeInitial, debugFramePing{})

	tc.wantIdle("after sending PTO probe, no additional frames to send")
}

func TestAck(t *testing.T) {
	tc := newTestConn(t, serverSide)
	tc.writeFrames(packetTypeInitial,
		debugFramePing{},
	)
	tc.wantFrame("connection should respond to ack-eliciting packet with an ACK frame",
		packetTypeInitial,
		debugFrameAck{
			ranges: []i64range[packetNumber]{{0, 1}},
		},
	)
}
