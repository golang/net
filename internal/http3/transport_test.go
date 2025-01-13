// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.24 && goexperiment.synctest

package http3

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"golang.org/x/net/quic"
)

func TestTransportCreatesControlStream(t *testing.T) {
	runSynctest(t, func(t testing.TB) {
		tc := newTestClientConn(t)
		controlStream := tc.wantStream(
			"client creates control stream",
			streamTypeControl)
		controlStream.wantFrameHeader(
			"client sends SETTINGS frame on control stream",
			frameTypeSettings)
		controlStream.discardFrame()
	})
}

func TestTransportUnknownUnidirectionalStream(t *testing.T) {
	// "Recipients of unknown stream types MUST either abort reading of the stream
	// or discard incoming data without further processing."
	// https://www.rfc-editor.org/rfc/rfc9114.html#section-6.2-7
	runSynctest(t, func(t testing.TB) {
		tc := newTestClientConn(t)
		tc.greet()

		st := tc.newStream(0x21) // reserved stream type

		// The client should send a STOP_SENDING for this stream,
		// but it should not close the connection.
		synctest.Wait()
		if _, err := st.Write([]byte("hello")); err == nil {
			t.Fatalf("write to send-only stream with an unknown type succeeded; want error")
		}
		tc.wantNotClosed("after receiving unknown unidirectional stream type")
	})
}

func TestTransportUnknownSettings(t *testing.T) {
	// "An implementation MUST ignore any [settings] parameter with
	// an identifier it does not understand."
	// https://www.rfc-editor.org/rfc/rfc9114.html#section-7.2.4-9
	runSynctest(t, func(t testing.TB) {
		tc := newTestClientConn(t)
		controlStream := tc.newStream(streamTypeControl)
		controlStream.writeSettings(0x1f+0x21, 0) // reserved settings type
		controlStream.Flush()
		tc.wantNotClosed("after receiving unknown settings")
	})
}

func TestTransportInvalidSettings(t *testing.T) {
	// "These reserved settings MUST NOT be sent, and their receipt MUST
	// be treated as a connection error of type H3_SETTINGS_ERROR."
	// https://www.rfc-editor.org/rfc/rfc9114.html#section-7.2.4.1-5
	runSynctest(t, func(t testing.TB) {
		tc := newTestClientConn(t)
		controlStream := tc.newStream(streamTypeControl)
		controlStream.writeSettings(0x02, 0) // HTTP/2 SETTINGS_ENABLE_PUSH
		controlStream.Flush()
		tc.wantClosed("invalid setting", errH3SettingsError)
	})
}

func TestTransportDuplicateStream(t *testing.T) {
	for _, stype := range []streamType{
		streamTypeControl,
		streamTypeEncoder,
		streamTypeDecoder,
	} {
		runSynctestSubtest(t, stype.String(), func(t testing.TB) {
			tc := newTestClientConn(t)
			_ = tc.newStream(stype)
			tc.wantNotClosed("after creating one " + stype.String() + " stream")

			// Opening a second control, encoder, or decoder stream
			// is a protocol violation.
			_ = tc.newStream(stype)
			tc.wantClosed("duplicate stream", errH3StreamCreationError)
		})
	}
}

func TestTransportUnknownFrames(t *testing.T) {
	for _, stype := range []streamType{
		streamTypeControl,
	} {
		runSynctestSubtest(t, stype.String(), func(t testing.TB) {
			tc := newTestClientConn(t)
			tc.greet()
			st := tc.peerUnidirectionalStream(stype)

			data := "frame content"
			st.writeVarint(0x1f + 0x21)      // reserved frame type
			st.writeVarint(int64(len(data))) // size
			st.Write([]byte(data))
			st.Flush()

			tc.wantNotClosed("after writing unknown frame")
		})
	}
}

func TestTransportInvalidFrames(t *testing.T) {
	runSynctest(t, func(t testing.TB) {
		tc := newTestClientConn(t)
		tc.greet()
		tc.control.writeVarint(int64(frameTypeData))
		tc.control.writeVarint(0) // size
		tc.control.Flush()
		tc.wantClosed("after writing DATA frame to control stream", errH3FrameUnexpected)
	})
}

func TestTransportServerCreatesBidirectionalStream(t *testing.T) {
	// "Clients MUST treat receipt of a server-initiated bidirectional
	// stream as a connection error of type H3_STREAM_CREATION_ERROR [...]"
	// https://www.rfc-editor.org/rfc/rfc9114.html#section-6.1-3
	runSynctest(t, func(t testing.TB) {
		tc := newTestClientConn(t)
		tc.greet()
		st := tc.newStream(streamTypeRequest)
		st.Flush()
		tc.wantClosed("after server creates bidi stream", errH3StreamCreationError)
	})
}

func TestTransportServerCreatesBadUnidirectionalStream(t *testing.T) {
	runSynctest(t, func(t testing.TB) {
		tc := newTestClientConn(t)
		tc.greet()

		// Create and close a stream without sending the unidirectional stream header.
		qs, err := tc.qconn.NewSendOnlyStream(canceledCtx)
		if err != nil {
			t.Fatal(err)
		}
		st := newTestQUICStream(tc.t, newStream(qs))
		st.stream.stream.Close()

		tc.wantClosed("after server creates and closes uni stream", errH3StreamCreationError)
	})
}

// A testQUICConn wraps a *quic.Conn and provides methods for inspecting it.
type testQUICConn struct {
	t       testing.TB
	qconn   *quic.Conn
	streams map[streamType][]*testQUICStream
}

func newTestQUICConn(t testing.TB, qconn *quic.Conn) *testQUICConn {
	tq := &testQUICConn{
		t:       t,
		qconn:   qconn,
		streams: make(map[streamType][]*testQUICStream),
	}

	go tq.acceptStreams(t.Context())

	t.Cleanup(func() {
		tq.qconn.Close()
	})
	return tq
}

func (tq *testQUICConn) acceptStreams(ctx context.Context) {
	for {
		qst, err := tq.qconn.AcceptStream(ctx)
		if err != nil {
			return
		}
		st := newStream(qst)
		stype := streamTypeRequest
		if qst.IsReadOnly() {
			v, err := st.readVarint()
			if err != nil {
				tq.t.Errorf("error reading stream type from unidirectional stream: %v", err)
				continue
			}
			stype = streamType(v)
		}
		tq.streams[stype] = append(tq.streams[stype], newTestQUICStream(tq.t, st))
	}
}

// wantNotClosed asserts that the peer has not closed the connectioln.
func (tq *testQUICConn) wantNotClosed(reason string) {
	t := tq.t
	t.Helper()
	synctest.Wait()
	err := tq.qconn.Wait(canceledCtx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v: want QUIC connection to be alive; closed with error: %v", reason, err)
	}
}

// wantClosed asserts that the peer has closed the connection
// with the provided error code.
func (tq *testQUICConn) wantClosed(reason string, want error) {
	t := tq.t
	t.Helper()
	synctest.Wait()

	if e, ok := want.(http3Error); ok {
		want = &quic.ApplicationError{Code: uint64(e)}
	}
	got := tq.qconn.Wait(canceledCtx)
	if errors.Is(got, context.Canceled) {
		t.Fatalf("%v: want QUIC connection closed, but it is not", reason)
	}
	if !errors.Is(got, want) {
		t.Fatalf("%v: connection closed with error: %v; want %v", reason, got, want)
	}
}

// wantStream asserts that a stream of a given type has been created,
// and returns that stream.
func (tq *testQUICConn) wantStream(reason string, stype streamType) *testQUICStream {
	tq.t.Helper()
	if len(tq.streams[stype]) == 0 {
		tq.t.Fatalf("%v: stream not created", reason)
	}
	ts := tq.streams[stype][0]
	tq.streams[stype] = tq.streams[stype][1:]
	return ts
}

// testQUICStream wraps a QUIC stream and provides methods for inspecting it.
type testQUICStream struct {
	t testing.TB
	*stream
}

func newTestQUICStream(t testing.TB, st *stream) *testQUICStream {
	st.stream.SetReadContext(canceledCtx)
	st.stream.SetWriteContext(canceledCtx)
	return &testQUICStream{
		t:      t,
		stream: st,
	}
}

// wantFrameHeader calls readFrameHeader and asserts that the frame is of a given type.
func (ts *testQUICStream) wantFrameHeader(reason string, wantType frameType) {
	ts.t.Helper()
	gotType, err := ts.readFrameHeader()
	if err != nil {
		ts.t.Fatalf("%v: failed to read frame header: %v", reason, err)
	}
	if gotType != wantType {
		ts.t.Fatalf("%v: got frame type %v, want %v", reason, gotType, wantType)
	}
}

func (ts *testQUICStream) Flush() error {
	err := ts.stream.Flush()
	if err != nil {
		ts.t.Errorf("unexpected error flushing stream: %v", err)
	}
	return err
}

// A testClientConn is a ClientConn on a test network.
type testClientConn struct {
	tr *Transport
	cc *ClientConn

	// *testQUICConn is the server half of the connection.
	*testQUICConn
	control *testQUICStream
}

func newTestClientConn(t testing.TB) *testClientConn {
	e1, e2 := newQUICEndpointPair(t)
	tr := &Transport{
		Endpoint: e1,
		Config: &quic.Config{
			TLSConfig: testTLSConfig,
		},
	}

	cc, err := tr.Dial(t.Context(), e2.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cc.Close()
	})
	srvConn, err := e2.Accept(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	tc := &testClientConn{
		tr:           tr,
		cc:           cc,
		testQUICConn: newTestQUICConn(t, srvConn),
	}
	synctest.Wait()
	return tc
}

// greet performs initial connection handshaking with the client.
func (tc *testClientConn) greet() {
	// Client creates a control stream.
	clientControlStream := tc.wantStream(
		"client creates control stream",
		streamTypeControl)
	clientControlStream.wantFrameHeader(
		"client sends SETTINGS frame on control stream",
		frameTypeSettings)
	clientControlStream.discardFrame()

	// Server creates a control stream.
	tc.control = tc.newStream(streamTypeControl)
	tc.control.writeVarint(int64(frameTypeSettings))
	tc.control.writeVarint(0) // size
	tc.control.Flush()

	synctest.Wait()
}

func (tc *testClientConn) newStream(stype streamType) *testQUICStream {
	tc.t.Helper()
	var qs *quic.Stream
	var err error
	if stype == streamTypeRequest {
		qs, err = tc.qconn.NewStream(canceledCtx)
	} else {
		qs, err = tc.qconn.NewSendOnlyStream(canceledCtx)
	}
	if err != nil {
		tc.t.Fatal(err)
	}
	st := newStream(qs)
	if stype != streamTypeRequest {
		st.writeVarint(int64(stype))
		if err := st.Flush(); err != nil {
			tc.t.Fatal(err)
		}
	}
	return newTestQUICStream(tc.t, st)
}

// peerUnidirectionalStream returns the peer-created unidirectional stream with
// the given type. It produces a fatal error if the peer hasn't created this stream.
func (tc *testClientConn) peerUnidirectionalStream(stype streamType) *testQUICStream {
	var st *testQUICStream
	switch stype {
	case streamTypeControl:
		st = tc.control
	}
	if st == nil {
		tc.t.Fatalf("want peer stream of type %v, but not created yet", stype)
	}
	return st
}

// canceledCtx is a canceled Context.
// Used for performing non-blocking QUIC operations.
var canceledCtx = func() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}()
