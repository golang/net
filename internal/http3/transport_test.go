// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.24 && goexperiment.synctest

package http3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"testing/synctest"

	"golang.org/x/net/internal/quic/quicwire"
	"golang.org/x/net/quic"
)

func TestTransportCreatesControlStream(t *testing.T) {
	runSynctest(t, func(t testing.TB) {
		tc := newTestClientConn(t)
		controlStream := tc.wantStream(streamTypeControl)
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
func (tq *testQUICConn) wantStream(stype streamType) *testQUICStream {
	tq.t.Helper()
	synctest.Wait()
	if len(tq.streams[stype]) == 0 {
		tq.t.Fatalf("expected a %v stream to be created, but none were", stype)
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
	synctest.Wait()
	gotType, err := ts.readFrameHeader()
	if err != nil {
		ts.t.Fatalf("%v: failed to read frame header: %v", reason, err)
	}
	if gotType != wantType {
		ts.t.Fatalf("%v: got frame type %v, want %v", reason, gotType, wantType)
	}
}

// wantHeaders reads a HEADERS frame.
// If want is nil, the contents of the frame are ignored.
func (ts *testQUICStream) wantHeaders(want http.Header) {
	ts.t.Helper()
	ftype, err := ts.readFrameHeader()
	if err != nil {
		ts.t.Fatalf("want HEADERS frame, got error: %v", err)
	}
	if ftype != frameTypeHeaders {
		ts.t.Fatalf("want HEADERS frame, got: %v", ftype)
	}

	if want == nil {
		if err := ts.discardFrame(); err != nil {
			ts.t.Fatalf("discardFrame: %v", err)
		}
		return
	}

	got := make(http.Header)
	var dec qpackDecoder
	err = dec.decode(ts.stream, func(_ indexType, name, value string) error {
		got.Add(name, value)
		return nil
	})
	if diff := diffHeaders(got, want); diff != "" {
		ts.t.Fatalf("unexpected response headers:\n%v", diff)
	}
	if err := ts.endFrame(); err != nil {
		ts.t.Fatalf("endFrame: %v", err)
	}
}

func (ts *testQUICStream) encodeHeaders(h http.Header) []byte {
	ts.t.Helper()
	var enc qpackEncoder
	return enc.encode(func(yield func(itype indexType, name, value string)) {
		names := slices.Collect(maps.Keys(h))
		slices.Sort(names)
		for _, k := range names {
			for _, v := range h[k] {
				yield(mayIndex, k, v)
			}
		}
	})
}

func (ts *testQUICStream) writeHeaders(h http.Header) {
	ts.t.Helper()
	headers := ts.encodeHeaders(h)
	ts.writeVarint(int64(frameTypeHeaders))
	ts.writeVarint(int64(len(headers)))
	ts.Write(headers)
	if err := ts.Flush(); err != nil {
		ts.t.Fatalf("flushing HEADERS frame: %v", err)
	}
}

func (ts *testQUICStream) wantData(want []byte) {
	ts.t.Helper()
	synctest.Wait()
	ftype, err := ts.readFrameHeader()
	if err != nil {
		ts.t.Fatalf("want DATA frame, got error: %v", err)
	}
	if ftype != frameTypeData {
		ts.t.Fatalf("want DATA frame, got: %v", ftype)
	}
	got, err := ts.readFrameData()
	if err != nil {
		ts.t.Fatalf("error reading DATA frame: %v", err)
	}
	if !bytes.Equal(got, want) {
		ts.t.Fatalf("got data: {%x}, want {%x}", got, want)
	}
	if err := ts.endFrame(); err != nil {
		ts.t.Fatalf("endFrame: %v", err)
	}
}

func (ts *testQUICStream) wantClosed(reason string) {
	ts.t.Helper()
	synctest.Wait()
	ftype, err := ts.readFrameHeader()
	if err != io.EOF {
		ts.t.Fatalf("%v: want io.EOF, got %v %v", reason, ftype, err)
	}
}

func (ts *testQUICStream) wantError(want quic.StreamErrorCode) {
	ts.t.Helper()
	synctest.Wait()
	_, err := ts.stream.stream.ReadByte()
	if err == nil {
		ts.t.Fatalf("successfully read from stream; want stream error code %v", want)
	}
	var got quic.StreamErrorCode
	if !errors.As(err, &got) {
		ts.t.Fatalf("stream error = %v; want %v", err, want)
	}
	if got != want {
		ts.t.Fatalf("stream error code = %v; want %v", got, want)
	}
}

func (ts *testQUICStream) writePushPromise(pushID int64, h http.Header) {
	ts.t.Helper()
	headers := ts.encodeHeaders(h)
	ts.writeVarint(int64(frameTypePushPromise))
	ts.writeVarint(int64(quicwire.SizeVarint(uint64(pushID)) + len(headers)))
	ts.writeVarint(pushID)
	ts.Write(headers)
	if err := ts.Flush(); err != nil {
		ts.t.Fatalf("flushing PUSH_PROMISE frame: %v", err)
	}
}

func diffHeaders(got, want http.Header) string {
	// nil and 0-length non-nil are equal.
	if len(got) == 0 && len(want) == 0 {
		return ""
	}
	// We could do a more sophisticated diff here.
	// DeepEqual is good enough for now.
	if reflect.DeepEqual(got, want) {
		return ""
	}
	return fmt.Sprintf("got:  %v\nwant: %v", got, want)
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
	clientControlStream := tc.wantStream(streamTypeControl)
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

type testRoundTrip struct {
	t       testing.TB
	resp    *http.Response
	respErr error
}

func (rt *testRoundTrip) done() bool {
	synctest.Wait()
	return rt.resp != nil || rt.respErr != nil
}

func (rt *testRoundTrip) result() (*http.Response, error) {
	rt.t.Helper()
	if !rt.done() {
		rt.t.Fatal("RoundTrip is not done; want it to be")
	}
	return rt.resp, rt.respErr
}

func (rt *testRoundTrip) response() *http.Response {
	rt.t.Helper()
	if !rt.done() {
		rt.t.Fatal("RoundTrip is not done; want it to be")
	}
	if rt.respErr != nil {
		rt.t.Fatalf("RoundTrip returned unexpected error: %v", rt.respErr)
	}
	return rt.resp
}

// err returns the (possibly nil) error result of RoundTrip.
func (rt *testRoundTrip) err() error {
	rt.t.Helper()
	_, err := rt.result()
	return err
}

func (rt *testRoundTrip) wantError(reason string) {
	rt.t.Helper()
	synctest.Wait()
	if !rt.done() {
		rt.t.Fatalf("%v: RoundTrip is not done; want it to have returned an error", reason)
	}
	if rt.respErr == nil {
		rt.t.Fatalf("%v: RoundTrip succeeded; want it to have returned an error", reason)
	}
}

// wantStatus indicates the expected response StatusCode.
func (rt *testRoundTrip) wantStatus(want int) {
	rt.t.Helper()
	if got := rt.response().StatusCode; got != want {
		rt.t.Fatalf("got response status %v, want %v", got, want)
	}
}

func (rt *testRoundTrip) wantHeaders(want http.Header) {
	rt.t.Helper()
	if diff := diffHeaders(rt.response().Header, want); diff != "" {
		rt.t.Fatalf("unexpected response headers:\n%v", diff)
	}
}

func (tc *testClientConn) roundTrip(req *http.Request) *testRoundTrip {
	rt := &testRoundTrip{t: tc.t}
	go func() {
		rt.resp, rt.respErr = tc.cc.RoundTrip(req)
	}()
	return rt
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
