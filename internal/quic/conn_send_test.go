// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"testing"
	"time"
)

func TestAckElicitingAck(t *testing.T) {
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
		tc.advance(1 * time.Millisecond)
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
