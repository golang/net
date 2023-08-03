// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type streamsState struct {
	queue queue[*Stream] // new, peer-created streams

	streamsMu sync.Mutex
	streams   map[streamID]*Stream
	opened    [streamTypeCount]int64 // number of streams opened by us

	// Streams with frames to send are stored in a circular linked list.
	// sendHead is the next stream to write, or nil if there are no streams
	// with data to send. sendTail is the last stream to write.
	needSend atomic.Bool
	sendMu   sync.Mutex
	sendHead *Stream
	sendTail *Stream
}

func (c *Conn) streamsInit() {
	c.streams.streams = make(map[streamID]*Stream)
	c.streams.queue = newQueue[*Stream]()
}

// AcceptStream waits for and returns the next stream created by the peer.
func (c *Conn) AcceptStream(ctx context.Context) (*Stream, error) {
	return c.streams.queue.getWithHooks(ctx, c.testHooks)
}

// NewStream creates a stream.
//
// If the peer's maximum stream limit for the connection has been reached,
// NewStream blocks until the limit is increased or the context expires.
func (c *Conn) NewStream(ctx context.Context) (*Stream, error) {
	return c.newLocalStream(ctx, bidiStream)
}

// NewSendOnlyStream creates a unidirectional, send-only stream.
//
// If the peer's maximum stream limit for the connection has been reached,
// NewSendOnlyStream blocks until the limit is increased or the context expires.
func (c *Conn) NewSendOnlyStream(ctx context.Context) (*Stream, error) {
	return c.newLocalStream(ctx, uniStream)
}

func (c *Conn) newLocalStream(ctx context.Context, typ streamType) (*Stream, error) {
	// TODO: Stream limits.
	c.streams.streamsMu.Lock()
	defer c.streams.streamsMu.Unlock()

	num := c.streams.opened[typ]
	c.streams.opened[typ]++

	s := newStream(c, newStreamID(c.side, typ, num))
	c.streams.streams[s.id] = s
	return s, nil
}

// streamFrameType identifies which direction of a stream,
// from the local perspective, a frame is associated with.
//
// For example, STREAM is a recvStream frame,
// because it carries data from the peer to us.
type streamFrameType uint8

const (
	sendStream = streamFrameType(iota) // for example, MAX_DATA
	recvStream                         // for example, STREAM_DATA_BLOCKED
)

// streamForID returns the stream with the given id.
// If the stream does not exist, it returns nil.
func (c *Conn) streamForID(id streamID) *Stream {
	c.streams.streamsMu.Lock()
	defer c.streams.streamsMu.Unlock()
	return c.streams.streams[id]
}

// streamForFrame returns the stream with the given id.
// If the stream does not exist, it may be created.
//
// streamForFrame aborts the connection if the stream id, state, and frame type don't align.
// For example, it aborts the connection with a STREAM_STATE error if a MAX_DATA frame
// is received for a receive-only stream, or if the peer attempts to create a stream that
// should be originated locally.
//
// streamForFrame returns nil if the stream no longer exists or if an error occurred.
func (c *Conn) streamForFrame(now time.Time, id streamID, ftype streamFrameType) *Stream {
	if id.streamType() == uniStream {
		if (id.initiator() == c.side) != (ftype == sendStream) {
			// Received an invalid frame for unidirectional stream.
			// For example, a RESET_STREAM frame for a send-only stream.
			c.abort(now, localTransportError(errStreamState))
			return nil
		}
	}

	c.streams.streamsMu.Lock()
	defer c.streams.streamsMu.Unlock()
	if s := c.streams.streams[id]; s != nil {
		return s
	}
	// TODO: Check for closed streams, once we support closing streams.
	if id.initiator() == c.side {
		c.abort(now, localTransportError(errStreamState))
		return nil
	}
	s := newStream(c, id)
	c.streams.streams[id] = s
	c.streams.queue.put(s)
	return s
}

// queueStreamForSend marks a stream as containing frames that need sending.
func (c *Conn) queueStreamForSend(s *Stream) {
	c.streams.sendMu.Lock()
	defer c.streams.sendMu.Unlock()
	if s.next != nil {
		// Already in the queue.
		return
	}
	if c.streams.sendHead == nil {
		// The queue was empty.
		c.streams.sendHead = s
		c.streams.sendTail = s
		s.next = s
	} else {
		// Insert this stream at the end of the queue.
		c.streams.sendTail.next = s
		c.streams.sendTail = s
	}
	c.streams.needSend.Store(true)
	c.wake()
}

// appendStreamFrames writes stream-related frames to the current packet.
//
// It returns true if no more frames need appending,
// false if not everything fit in the current packet.
func (c *Conn) appendStreamFrames(w *packetWriter, pnum packetNumber, pto bool) bool {
	if pto {
		return c.appendStreamFramesPTO(w, pnum)
	}
	if !c.streams.needSend.Load() {
		return true
	}
	c.streams.sendMu.Lock()
	defer c.streams.sendMu.Unlock()
	for {
		s := c.streams.sendHead
		const pto = false
		if !s.appendInFrames(w, pnum, pto) {
			return false
		}
		avail := w.avail()
		if !s.appendOutFrames(w, pnum, pto) {
			// We've sent some data for this stream, but it still has more to send.
			// If the stream got a reasonable chance to put data in a packet,
			// advance sendHead to the next stream in line, to avoid starvation.
			// We'll come back to this stream after going through the others.
			//
			// If the packet was already mostly out of space, leave sendHead alone
			// and come back to this stream again on the next packet.
			if avail > 512 {
				c.streams.sendHead = s.next
				c.streams.sendTail = s
			}
			return false
		}
		s.next = nil
		if s == c.streams.sendTail {
			// This was the last stream.
			c.streams.sendHead = nil
			c.streams.sendTail = nil
			c.streams.needSend.Store(false)
			return true
		}
		// We've sent all data for this stream, so remove it from the list.
		c.streams.sendTail.next = s.next
		c.streams.sendHead = s.next
		s.next = nil
	}
}

// appendStreamFramesPTO writes stream-related frames to the current packet
// for a PTO probe.
//
// It returns true if no more frames need appending,
// false if not everything fit in the current packet.
func (c *Conn) appendStreamFramesPTO(w *packetWriter, pnum packetNumber) bool {
	c.streams.sendMu.Lock()
	defer c.streams.sendMu.Unlock()
	for _, s := range c.streams.streams {
		const pto = true
		if !s.appendInFrames(w, pnum, pto) {
			return false
		}
		if !s.appendOutFrames(w, pnum, pto) {
			return false
		}
	}
	return true
}
