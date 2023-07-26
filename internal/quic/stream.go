// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"context"
	"errors"
)

type Stream struct {
	id   streamID
	conn *Conn

	// outgate's lock guards all send-related state.
	//
	// The gate condition is set if a write to the stream will not block,
	// either because the stream has available flow control or because
	// the write will fail.
	outgate   gate
	outopened sentVal // set if we should open the stream

	prev, next *Stream // guarded by streamsState.sendMu
}

func newStream(c *Conn, id streamID) *Stream {
	s := &Stream{
		conn:    c,
		id:      id,
		outgate: newGate(),
	}

	// Lock and unlock outgate to update the stream writability state.
	s.outgate.lock()
	s.outUnlock()

	return s
}

// IsReadOnly reports whether the stream is read-only
// (a unidirectional stream created by the peer).
func (s *Stream) IsReadOnly() bool {
	return s.id.streamType() == uniStream && s.id.initiator() != s.conn.side
}

// IsWriteOnly reports whether the stream is write-only
// (a unidirectional stream created locally).
func (s *Stream) IsWriteOnly() bool {
	return s.id.streamType() == uniStream && s.id.initiator() == s.conn.side
}

// Read reads data from the stream.
// See ReadContext for more details.
func (s *Stream) Read(b []byte) (n int, err error) {
	return s.ReadContext(context.Background(), b)
}

// ReadContext reads data from the stream.
//
// ReadContext returns as soon as at least one byte of data is available.
//
// If the peer closes the stream cleanly, ReadContext returns io.EOF after
// returning all data sent by the peer.
// If the peer terminates reads abruptly, ReadContext returns StreamResetError.
func (s *Stream) ReadContext(ctx context.Context, b []byte) (n int, err error) {
	// TODO: implement
	return 0, errors.New("unimplemented")
}

// Write writes data to the stream.
// See WriteContext for more details.
func (s *Stream) Write(b []byte) (n int, err error) {
	return s.WriteContext(context.Background(), b)
}

// WriteContext writes data to the stream.
//
// WriteContext writes data to the stream write buffer.
// Buffered data is only sent when the buffer is sufficiently full.
// Call the Flush method to ensure buffered data is sent.
//
// If the peer aborts reads on the stream, ReadContext returns StreamResetError.
func (s *Stream) WriteContext(ctx context.Context, b []byte) (n int, err error) {
	if s.IsReadOnly() {
		return 0, errors.New("write to read-only stream")
	}
	if len(b) > 0 {
		// TODO: implement
		return 0, errors.New("unimplemented")
	}
	if err := s.outgate.waitAndLockContext(ctx); err != nil {
		return 0, err
	}
	defer s.outUnlock()

	// Set outopened to send a STREAM frame with no data,
	// opening the stream on the peer.
	s.outopened.set()

	return n, nil
}

// outUnlock unlocks s.outgate.
// It sets the gate condition if writes to s will not block.
// If s has frames to write, it notifies the Conn.
func (s *Stream) outUnlock() {
	if s.outopened.shouldSend() {
		s.conn.queueStreamForSend(s)
	}
	canSend := true // TODO: set sendability status based on flow control
	s.outgate.unlock(canSend)
}

// handleData handles data received in a STREAM frame.
func (s *Stream) handleData(off int64, b []byte, fin bool) error {
	// TODO
	return nil
}

// ackOrLossData handles the fate of a STREAM frame.
func (s *Stream) ackOrLossData(pnum packetNumber, start, end int64, fin bool, fate packetFate) {
	s.outgate.lock()
	defer s.outUnlock()
	s.outopened.ackOrLoss(pnum, fate)
}

func (s *Stream) appendInFrames(w *packetWriter, pnum packetNumber, pto bool) bool {
	// TODO: STOP_SENDING
	// TODO: MAX_STREAM_DATA
	return true
}

func (s *Stream) appendOutFrames(w *packetWriter, pnum packetNumber, pto bool) bool {
	// TODO: RESET_STREAM
	// TODO: STREAM_DATA_BLOCKED
	// TODO: STREAM frames with data
	if s.outopened.shouldSendPTO(pto) {
		off := int64(0)
		size := 0
		fin := false
		_, added := w.appendStreamFrame(s.id, off, size, fin)
		if !added {
			return false
		}
		s.outopened.setSent(pnum)
	}
	return true
}
