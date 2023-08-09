// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"context"
	"testing"
)

func TestStreamsCreate(t *testing.T) {
	ctx := canceledContext()
	tc := newTestConn(t, clientSide, func(p *transportParameters) {
		p.initialMaxStreamDataBidiLocal = 100
		p.initialMaxStreamDataBidiRemote = 100
	})
	tc.handshake()

	c, err := tc.conn.NewStream(ctx)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	c.Write(nil) // open the stream
	tc.wantFrame("created bidirectional stream 0",
		packetType1RTT, debugFrameStream{
			id:   0, // client-initiated, bidi, number 0
			data: []byte{},
		})

	c, err = tc.conn.NewSendOnlyStream(ctx)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	c.Write(nil) // open the stream
	tc.wantFrame("created unidirectional stream 0",
		packetType1RTT, debugFrameStream{
			id:   2, // client-initiated, uni, number 0
			data: []byte{},
		})

	c, err = tc.conn.NewStream(ctx)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	c.Write(nil) // open the stream
	tc.wantFrame("created bidirectional stream 1",
		packetType1RTT, debugFrameStream{
			id:   4, // client-initiated, uni, number 4
			data: []byte{},
		})
}

func TestStreamsAccept(t *testing.T) {
	ctx := canceledContext()
	tc := newTestConn(t, serverSide)
	tc.handshake()

	tc.writeFrames(packetType1RTT,
		debugFrameStream{
			id: 0, // client-initiated, bidi, number 0
		},
		debugFrameStream{
			id: 2, // client-initiated, uni, number 0
		},
		debugFrameStream{
			id: 4, // client-initiated, bidi, number 1
		})

	for _, accept := range []struct {
		id       streamID
		readOnly bool
	}{
		{0, false},
		{2, true},
		{4, false},
	} {
		s, err := tc.conn.AcceptStream(ctx)
		if err != nil {
			t.Fatalf("conn.AcceptStream() = %v, want stream %v", err, accept.id)
		}
		if got, want := s.id, accept.id; got != want {
			t.Fatalf("conn.AcceptStream() = stream %v, want %v", got, want)
		}
		if got, want := s.IsReadOnly(), accept.readOnly; got != want {
			t.Fatalf("stream %v: s.IsReadOnly() = %v, want %v", accept.id, got, want)
		}
	}

	_, err := tc.conn.AcceptStream(ctx)
	if err != context.Canceled {
		t.Fatalf("conn.AcceptStream() = %v, want context.Canceled", err)
	}
}

func TestStreamsBlockingAccept(t *testing.T) {
	tc := newTestConn(t, serverSide)
	tc.handshake()

	a := runAsync(tc, func(ctx context.Context) (*Stream, error) {
		return tc.conn.AcceptStream(ctx)
	})
	if _, err := a.result(); err != errNotDone {
		tc.t.Fatalf("AcceptStream() = _, %v; want errNotDone", err)
	}

	sid := newStreamID(clientSide, bidiStream, 0)
	tc.writeFrames(packetType1RTT,
		debugFrameStream{
			id: sid,
		})

	s, err := a.result()
	if err != nil {
		t.Fatalf("conn.AcceptStream() = _, %v, want stream", err)
	}
	if got, want := s.id, sid; got != want {
		t.Fatalf("conn.AcceptStream() = stream %v, want %v", got, want)
	}
	if got, want := s.IsReadOnly(), false; got != want {
		t.Fatalf("s.IsReadOnly() = %v, want %v", got, want)
	}
}

func TestStreamsStreamNotCreated(t *testing.T) {
	// "An endpoint MUST terminate the connection with error STREAM_STATE_ERROR
	// if it receives a STREAM frame for a locally initiated stream that has
	// not yet been created [...]"
	// https://www.rfc-editor.org/rfc/rfc9000.html#section-19.8-3
	tc := newTestConn(t, serverSide)
	tc.handshake()

	tc.writeFrames(packetType1RTT,
		debugFrameStream{
			id: 1, // server-initiated, bidi, number 0
		})
	tc.wantFrame("peer sent STREAM frame for an uncreated local stream",
		packetType1RTT, debugFrameConnectionCloseTransport{
			code: errStreamState,
		})
}

func TestStreamsStreamSendOnly(t *testing.T) {
	// "An endpoint MUST terminate the connection with error STREAM_STATE_ERROR
	// if it receives a STREAM frame for a locally initiated stream that has
	// not yet been created [...]"
	// https://www.rfc-editor.org/rfc/rfc9000.html#section-19.8-3
	ctx := canceledContext()
	tc := newTestConn(t, serverSide)
	tc.handshake()

	c, err := tc.conn.NewSendOnlyStream(ctx)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	c.Write(nil) // open the stream
	tc.wantFrame("created unidirectional stream 0",
		packetType1RTT, debugFrameStream{
			id:   3, // server-initiated, uni, number 0
			data: []byte{},
		})

	tc.writeFrames(packetType1RTT,
		debugFrameStream{
			id: 3, // server-initiated, bidi, number 0
		})
	tc.wantFrame("peer sent STREAM frame for a send-only stream",
		packetType1RTT, debugFrameConnectionCloseTransport{
			code: errStreamState,
		})
}

func TestStreamsWriteQueueFairness(t *testing.T) {
	ctx := canceledContext()
	const dataLen = 1 << 20
	const numStreams = 3
	tc := newTestConn(t, clientSide, func(p *transportParameters) {
		p.initialMaxStreamsBidi = numStreams
		p.initialMaxData = 1<<62 - 1
		p.initialMaxStreamDataBidiRemote = dataLen
	}, func(c *Config) {
		c.StreamWriteBufferSize = dataLen
	})
	tc.handshake()
	tc.ignoreFrame(frameTypeAck)

	// Create a number of streams, and write a bunch of data to them.
	// The streams are not limited by flow control.
	//
	// The first stream we create is going to immediately consume all
	// available congestion window.
	//
	// Once we've created all the remaining streams,
	// we start sending acks back to open up the congestion window.
	// We verify that all streams can make progress.
	data := make([]byte, dataLen)
	var streams []*Stream
	for i := 0; i < numStreams; i++ {
		s, err := tc.conn.NewStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, s)
		if n, err := s.WriteContext(ctx, data); n != len(data) || err != nil {
			t.Fatalf("s.WriteContext() = %v, %v; want %v, nil", n, err, len(data))
		}
		// Wait for the stream to finish writing whatever frames it can before
		// congestion control blocks it.
		tc.wait()
	}

	sent := make([]int64, len(streams))
	for {
		p := tc.readPacket()
		if p == nil {
			break
		}
		tc.writeFrames(packetType1RTT, debugFrameAck{
			ranges: []i64range[packetNumber]{{0, p.num}},
		})
		for _, f := range p.frames {
			sf, ok := f.(debugFrameStream)
			if !ok {
				t.Fatalf("got unexpected frame (want STREAM): %v", sf)
			}
			if got, want := sf.off, sent[sf.id.num()]; got != want {
				t.Fatalf("got frame: %v\nwant offset: %v", sf, want)
			}
			sent[sf.id.num()] = sf.off + int64(len(sf.data))
			// Look at the amount of data sent by all streams, excluding the first one.
			// (The first stream got a head start when it consumed the initial window.)
			//
			// We expect that difference between the streams making the most and least progress
			// so far will be less than the maximum datagram size.
			minSent := sent[1]
			maxSent := sent[1]
			for _, s := range sent[2:] {
				minSent = min(minSent, s)
				maxSent = max(maxSent, s)
			}
			const maxDelta = maxUDPPayloadSize
			if d := maxSent - minSent; d > maxDelta {
				t.Fatalf("stream data sent: %v; delta=%v, want delta <= %v", sent, d, maxDelta)
			}
		}
	}
	// Final check that every stream sent the full amount of data expected.
	for num, s := range sent {
		if s != dataLen {
			t.Errorf("stream %v sent %v bytes, want %v", num, s, dataLen)
		}
	}
}
