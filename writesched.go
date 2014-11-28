// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package http2

// frameWriteMsg is a request to write a frame.
type frameWriteMsg struct {
	// write is the interface value that does the writing, once the
	// writeScheduler (below) has decided to select this frame
	// to write. The write functions are all defined in write.go.
	write writeFramer

	stream *stream // used for prioritization. nil for non-stream frames.

	// done, if non-nil, must be a buffered channel with space for
	// 1 message and is sent the return value from write (or an
	// earlier error) when the frame has been written.
	done chan error
}

// writeScheduler tracks pending frames to write, priorities, and decides
// the next one to use. It is not thread-safe.
type writeScheduler struct {
	// zero are frames not associated with a specific stream.
	// They're sent before any stream-specific freams.
	zero writeQueue

	// sq contains the stream-specific queues, keyed by stream ID.
	// when a stream is idle, it's deleted from the map.
	sq map[uint32]*writeQueue
}

func (ws *writeScheduler) empty() bool { return ws.zero.empty() && len(ws.sq) == 0 }

func (ws *writeScheduler) add(wm frameWriteMsg) {
	st := wm.stream
	if st == nil {
		ws.zero.push(wm)
	} else {
		ws.streamQueue(st.id).push(wm)
	}
}

func (ws *writeScheduler) streamQueue(streamID uint32) *writeQueue {
	if q, ok := ws.sq[streamID]; ok {
		return q
	}
	if ws.sq == nil {
		ws.sq = make(map[uint32]*writeQueue)
	}
	q := new(writeQueue)
	ws.sq[streamID] = q
	return q
}

// take returns the most important frame to write and removes it from the scheduler.
// It is illegal to call this if the scheduler is empty.
func (ws *writeScheduler) take() frameWriteMsg {
	// If there any frames not associated with streams, prefer those first.
	// These are usually SETTINGS, etc.
	if !ws.zero.empty() {
		return ws.zero.shift()
	}

	if len(ws.sq) == 0 {
		panic("internal error: take should only be called if non-empty")
	}

	// Next, prioritize frames on streams that aren't DATA frames (no cost).
	for id, q := range ws.sq {
		if q.firstIsNoCost() {
			return ws.takeFrom(id, q)
		}
	}

	// Now, all that remains are DATA frames. So pick the best one.
	// TODO: do that. For now, pick a random one.
	for id, q := range ws.sq {
		return ws.takeFrom(id, q)
	}
	panic("internal error: take should only be called if non-empty")
}

func (ws *writeScheduler) takeFrom(id uint32, q *writeQueue) frameWriteMsg {
	wm := q.shift()
	if q.empty() {
		// TODO: reclaim its slice and use it for future allocations
		// in the writeScheduler.streamQueue method above when making
		// the writeQueue.
		delete(ws.sq, id)
	}
	return wm
}

type writeQueue struct {
	s []frameWriteMsg
}

func (q *writeQueue) empty() bool { return len(q.s) == 0 }

func (q *writeQueue) push(wm frameWriteMsg) {
	q.s = append(q.s, wm)
}

func (q *writeQueue) shift() frameWriteMsg {
	if len(q.s) == 0 {
		panic("invalid use of queue")
	}
	wm := q.s[0]
	// TODO: less copy-happy queue.
	copy(q.s, q.s[1:])
	q.s[len(q.s)-1] = frameWriteMsg{}
	q.s = q.s[:len(q.s)-1]
	return wm
}

func (q *writeQueue) firstIsNoCost() bool {
	if df, ok := q.s[0].write.(*writeData); ok {
		return len(df.p) == 0
	}
	return true
}
