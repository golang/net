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

	// Limits on the number of streams, indexed by streamType.
	localLimit  [streamTypeCount]localStreamLimits
	remoteLimit [streamTypeCount]remoteStreamLimits

	// Peer configuration provided in transport parameters.
	peerInitialMaxStreamDataRemote    [streamTypeCount]int64 // streams opened by us
	peerInitialMaxStreamDataBidiLocal int64                  // streams opened by them

	// Connection-level flow control.
	inflow connInflow

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
	c.streams.localLimit[bidiStream].init()
	c.streams.localLimit[uniStream].init()
	c.streams.remoteLimit[bidiStream].init(c.config.maxBidiRemoteStreams())
	c.streams.remoteLimit[uniStream].init(c.config.maxUniRemoteStreams())
	c.inflowInit()
}

// AcceptStream waits for and returns the next stream created by the peer.
func (c *Conn) AcceptStream(ctx context.Context) (*Stream, error) {
	return c.streams.queue.get(ctx, c.testHooks)
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

func (c *Conn) newLocalStream(ctx context.Context, styp streamType) (*Stream, error) {
	c.streams.streamsMu.Lock()
	defer c.streams.streamsMu.Unlock()

	num, err := c.streams.localLimit[styp].open(ctx, c)
	if err != nil {
		return nil, err
	}

	s := newStream(c, newStreamID(c.side, styp, num))
	s.outmaxbuf = c.config.maxStreamWriteBufferSize()
	s.outwin = c.streams.peerInitialMaxStreamDataRemote[styp]
	if styp == bidiStream {
		s.inmaxbuf = c.config.maxStreamReadBufferSize()
		s.inwin = c.config.maxStreamReadBufferSize()
	}
	s.inUnlock()
	s.outUnlock()

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
	s, isOpen := c.streams.streams[id]
	if s != nil {
		return s
	}

	num := id.num()
	styp := id.streamType()
	if id.initiator() == c.side {
		if num < c.streams.localLimit[styp].opened {
			// This stream was created by us, and has been closed.
			return nil
		}
		// Received a frame for a stream that should be originated by us,
		// but which we never created.
		c.abort(now, localTransportError(errStreamState))
		return nil
	} else {
		// if isOpen, this is a stream that was implicitly opened by a
		// previous frame for a larger-numbered stream, but we haven't
		// actually created it yet.
		if !isOpen && num < c.streams.remoteLimit[styp].opened {
			// This stream was created by the peer, and has been closed.
			return nil
		}
	}

	prevOpened := c.streams.remoteLimit[styp].opened
	if err := c.streams.remoteLimit[styp].open(id); err != nil {
		c.abort(now, err)
		return nil
	}

	// Receiving a frame for a stream implicitly creates all streams
	// with the same initiator and type and a lower number.
	// Add a nil entry to the streams map for each implicitly created stream.
	for n := newStreamID(id.initiator(), id.streamType(), prevOpened); n < id; n += 4 {
		c.streams.streams[n] = nil
	}

	s = newStream(c, id)
	s.inmaxbuf = c.config.maxStreamReadBufferSize()
	s.inwin = c.config.maxStreamReadBufferSize()
	if id.streamType() == bidiStream {
		s.outmaxbuf = c.config.maxStreamWriteBufferSize()
		s.outwin = c.streams.peerInitialMaxStreamDataBidiLocal
	}
	s.inUnlock()
	s.outUnlock()

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
		s.next = c.streams.sendHead
	}
	c.streams.needSend.Store(true)
	c.wake()
}

// appendStreamFrames writes stream-related frames to the current packet.
//
// It returns true if no more frames need appending,
// false if not everything fit in the current packet.
func (c *Conn) appendStreamFrames(w *packetWriter, pnum packetNumber, pto bool) bool {
	// MAX_DATA
	if !c.appendMaxDataFrame(w, pnum, pto) {
		return false
	}

	// MAX_STREAM_DATA
	if !c.streams.remoteLimit[uniStream].appendFrame(w, uniStream, pnum, pto) {
		return false
	}
	if !c.streams.remoteLimit[bidiStream].appendFrame(w, bidiStream, pnum, pto) {
		return false
	}

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

		state := s.state.load()
		if state&streamInSend != 0 {
			s.ingate.lock()
			ok := s.appendInFramesLocked(w, pnum, pto)
			state = s.inUnlockNoQueue()
			if !ok {
				return false
			}
		}

		if state&streamOutSend != 0 {
			avail := w.avail()
			s.outgate.lock()
			ok := s.appendOutFramesLocked(w, pnum, pto)
			state = s.outUnlockNoQueue()
			if !ok {
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
		}

		if state == streamInDone|streamOutDone {
			// Stream is finished, remove it from the conn.
			s.state.set(streamConnRemoved, streamConnRemoved)
			delete(c.streams.streams, s.id)

			// Record finalization of remote streams, to know when
			// to extend the peer's stream limit.
			if s.id.initiator() != c.side {
				c.streams.remoteLimit[s.id.streamType()].close()
			}
		}

		next := s.next
		s.next = nil
		if (next == s) != (s == c.streams.sendTail) {
			panic("BUG: sendable stream list state is inconsistent")
		}
		if s == c.streams.sendTail {
			// This was the last stream.
			c.streams.sendHead = nil
			c.streams.sendTail = nil
			c.streams.needSend.Store(false)
			return true
		}
		// We've sent all data for this stream, so remove it from the list.
		c.streams.sendTail.next = next
		c.streams.sendHead = next
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
	const pto = true
	for _, s := range c.streams.streams {
		const pto = true
		s.ingate.lock()
		inOK := s.appendInFramesLocked(w, pnum, pto)
		s.inUnlockNoQueue()
		if !inOK {
			return false
		}

		s.outgate.lock()
		outOK := s.appendOutFramesLocked(w, pnum, pto)
		s.outUnlockNoQueue()
		if !outOK {
			return false
		}
	}
	return true
}
