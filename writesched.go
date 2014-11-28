// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package http2

// writeScheduler tracks pending frames to write, priorities, and decides
// the next one to use. It is not thread-safe.
type writeScheduler struct {
	slice []frameWriteMsg
}

func (ws *writeScheduler) empty() bool { return len(ws.slice) == 0 }

func (ws *writeScheduler) add(wm frameWriteMsg) {
	ws.slice = append(ws.slice, wm)
}

// take returns
func (ws *writeScheduler) take() frameWriteMsg {
	if ws.empty() {
		panic("internal error: writeScheduler.take called when empty")
	}
	// TODO:
	// -- prioritize all non-DATA frames first. they're not flow controlled anyway and
	//    they're generally more important.
	// -- for all DATA frames that are enqueued (and we should enqueue []byte instead of FRAMES),
	//    go over each (in priority order, as determined by the whole priority tree chaos),
	//    and decide which we have tokens for, and how many tokens.

	// Writing on stream X requires that we have tokens on the
	// stream 0 (the conn-as-a-whole stream) as well as stream X.

	// So: find the highest priority stream X, then see: do we
	// have tokens for X? Let's say we have N_X tokens. Then we should
	// write MIN(N_X, TOKENS(conn-wide-tokens)).
	//
	// Any tokens left over? Repeat. Well, not really... the
	// repeat will happen via the next call to
	// scheduleFrameWrite. So keep a HEAP (priqueue) of which
	// streams to write to.

	// TODO: proper scheduler
	wm := ws.slice[0]
	// shift it all down. kinda lame. will be removed later anyway.
	copy(ws.slice, ws.slice[1:])
	ws.slice[len(ws.slice)-1] = frameWriteMsg{}
	ws.slice = ws.slice[:len(ws.slice)-1]
	return wm
}
