// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"reflect"
	"testing"
)

func TestStreamOffsetTooLarge(t *testing.T) {
	// "Receipt of a frame that exceeds [2^62-1] MUST be treated as a
	// connection error of type FRAME_ENCODING_ERROR or FLOW_CONTROL_ERROR."
	// https://www.rfc-editor.org/rfc/rfc9000.html#section-19.8-9
	tc := newTestConn(t, serverSide)
	tc.handshake()

	tc.writeFrames(packetType1RTT,
		debugFrameStream{
			id:   newStreamID(clientSide, bidiStream, 0),
			off:  1<<62 - 1,
			data: []byte{0},
		})
	got, _ := tc.readFrame()
	want1 := debugFrameConnectionCloseTransport{code: errFrameEncoding}
	want2 := debugFrameConnectionCloseTransport{code: errFlowControl}
	if !reflect.DeepEqual(got, want1) && !reflect.DeepEqual(got, want2) {
		t.Fatalf("STREAM offset exceeds 2^62-1\ngot:  %v\nwant: %v\n  or: %v", got, want1, want2)
	}
}
