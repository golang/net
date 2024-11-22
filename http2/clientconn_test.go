// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Infrastructure for testing ClientConn.RoundTrip.
// Put actual tests in transport_test.go.

package http2

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

// TestTestClientConn demonstrates usage of testClientConn.
func TestTestClientConn(t *testing.T) {
	// newTestClientConn creates a *ClientConn and surrounding test infrastructure.
	tc := newTestClientConn(t)

	// tc.greet reads the client's initial SETTINGS and WINDOW_UPDATE frames,
	// and sends a SETTINGS frame to the client.
	//
	// Additional settings may be provided as optional parameters to greet.
	tc.greet()

	// Request bodies must either be constant (bytes.Buffer, strings.Reader)
	// or created with newRequestBody.
	body := tc.newRequestBody()
	body.writeBytes(10)         // 10 arbitrary bytes...
	body.closeWithError(io.EOF) // ...followed by EOF.

	// tc.roundTrip calls RoundTrip, but does not wait for it to return.
	// It returns a testRoundTrip.
	req, _ := http.NewRequest("PUT", "https://dummy.tld/", body)
	rt := tc.roundTrip(req)

	// tc has a number of methods to check for expected frames sent.
	// Here, we look for headers and the request body.
	tc.wantHeaders(wantHeader{
		streamID:  rt.streamID(),
		endStream: false,
		header: http.Header{
			":authority": []string{"dummy.tld"},
			":method":    []string{"PUT"},
			":path":      []string{"/"},
		},
	})
	// Expect 10 bytes of request body in DATA frames.
	tc.wantData(wantData{
		streamID:  rt.streamID(),
		endStream: true,
		size:      10,
		multiple:  true,
	})

	// tc.writeHeaders sends a HEADERS frame back to the client.
	tc.writeHeaders(HeadersFrameParam{
		StreamID:   rt.streamID(),
		EndHeaders: true,
		EndStream:  true,
		BlockFragment: tc.makeHeaderBlockFragment(
			":status", "200",
		),
	})

	// Now that we've received headers, RoundTrip has finished.
	// testRoundTrip has various methods to examine the response,
	// or to fetch the response and/or error returned by RoundTrip
	rt.wantStatus(200)
	rt.wantBody(nil)
}

// A testClientConn allows testing ClientConn.RoundTrip against a fake server.
//
// A test using testClientConn consists of:
//   - actions on the client (calling RoundTrip, making data available to Request.Body);
//   - validation of frames sent by the client to the server; and
//   - providing frames from the server to the client.
//
// testClientConn manages synchronization, so tests can generally be written as
// a linear sequence of actions and validations without additional synchronization.
type testClientConn struct {
	t *testing.T

	tr    *Transport
	fr    *Framer
	cc    *ClientConn
	group *synctestGroup
	testConnFramer

	encbuf bytes.Buffer
	enc    *hpack.Encoder

	roundtrips []*testRoundTrip

	netconn *synctestNetConn
}

func newTestClientConnFromClientConn(t *testing.T, cc *ClientConn) *testClientConn {
	tc := &testClientConn{
		t:     t,
		tr:    cc.t,
		cc:    cc,
		group: cc.t.transportTestHooks.group.(*synctestGroup),
	}

	// srv is the side controlled by the test.
	var srv *synctestNetConn
	if cc.tconn == nil {
		// If cc.tconn is nil, we're being called with a new conn created by the
		// Transport's client pool. This path skips dialing the server, and we
		// create a test connection pair here.
		cc.tconn, srv = synctestNetPipe(tc.group)
	} else {
		// If cc.tconn is non-nil, we're in a test which provides a conn to the
		// Transport via a TLSNextProto hook. Extract the test connection pair.
		if tc, ok := cc.tconn.(*tls.Conn); ok {
			// Unwrap any *tls.Conn to the underlying net.Conn,
			// to avoid dealing with encryption in tests.
			cc.tconn = tc.NetConn()
		}
		srv = cc.tconn.(*synctestNetConn).peer
	}

	srv.SetReadDeadline(tc.group.Now())
	srv.autoWait = true
	tc.netconn = srv
	tc.enc = hpack.NewEncoder(&tc.encbuf)
	tc.fr = NewFramer(srv, srv)
	tc.testConnFramer = testConnFramer{
		t:   t,
		fr:  tc.fr,
		dec: hpack.NewDecoder(initialHeaderTableSize, nil),
	}
	tc.fr.SetMaxReadFrameSize(10 << 20)
	t.Cleanup(func() {
		tc.closeWrite()
	})

	return tc
}

func (tc *testClientConn) readClientPreface() {
	tc.t.Helper()
	// Read the client's HTTP/2 preface, sent prior to any HTTP/2 frames.
	buf := make([]byte, len(clientPreface))
	if _, err := io.ReadFull(tc.netconn, buf); err != nil {
		tc.t.Fatalf("reading preface: %v", err)
	}
	if !bytes.Equal(buf, clientPreface) {
		tc.t.Fatalf("client preface: %q, want %q", buf, clientPreface)
	}
}

func newTestClientConn(t *testing.T, opts ...any) *testClientConn {
	t.Helper()

	tt := newTestTransport(t, opts...)
	const singleUse = false
	_, err := tt.tr.newClientConn(nil, singleUse)
	if err != nil {
		t.Fatalf("newClientConn: %v", err)
	}

	return tt.getConn()
}

// sync waits for the ClientConn under test to reach a stable state,
// with all goroutines blocked on some input.
func (tc *testClientConn) sync() {
	tc.group.Wait()
}

// advance advances synthetic time by a duration.
func (tc *testClientConn) advance(d time.Duration) {
	tc.group.AdvanceTime(d)
	tc.sync()
}

// hasFrame reports whether a frame is available to be read.
func (tc *testClientConn) hasFrame() bool {
	return len(tc.netconn.Peek()) > 0
}

// isClosed reports whether the peer has closed the connection.
func (tc *testClientConn) isClosed() bool {
	return tc.netconn.IsClosedByPeer()
}

// closeWrite causes the net.Conn used by the ClientConn to return a error
// from Read calls.
func (tc *testClientConn) closeWrite() {
	tc.netconn.Close()
}

// testRequestBody is a Request.Body for use in tests.
type testRequestBody struct {
	tc   *testClientConn
	gate gate

	// At most one of buf or bytes can be set at any given time:
	buf   bytes.Buffer // specific bytes to read from the body
	bytes int          // body contains this many arbitrary bytes

	err error // read error (comes after any available bytes)
}

func (tc *testClientConn) newRequestBody() *testRequestBody {
	b := &testRequestBody{
		tc:   tc,
		gate: newGate(),
	}
	return b
}

func (b *testRequestBody) unlock() {
	b.gate.unlock(b.buf.Len() > 0 || b.bytes > 0 || b.err != nil)
}

// Read is called by the ClientConn to read from a request body.
func (b *testRequestBody) Read(p []byte) (n int, _ error) {
	if err := b.gate.waitAndLock(context.Background()); err != nil {
		return 0, err
	}
	defer b.unlock()
	switch {
	case b.buf.Len() > 0:
		return b.buf.Read(p)
	case b.bytes > 0:
		if len(p) > b.bytes {
			p = p[:b.bytes]
		}
		b.bytes -= len(p)
		for i := range p {
			p[i] = 'A'
		}
		return len(p), nil
	default:
		return 0, b.err
	}
}

// Close is called by the ClientConn when it is done reading from a request body.
func (b *testRequestBody) Close() error {
	return nil
}

// writeBytes adds n arbitrary bytes to the body.
func (b *testRequestBody) writeBytes(n int) {
	defer b.tc.sync()
	b.gate.lock()
	defer b.unlock()
	b.bytes += n
	b.checkWrite()
	b.tc.sync()
}

// Write adds bytes to the body.
func (b *testRequestBody) Write(p []byte) (int, error) {
	defer b.tc.sync()
	b.gate.lock()
	defer b.unlock()
	n, err := b.buf.Write(p)
	b.checkWrite()
	return n, err
}

func (b *testRequestBody) checkWrite() {
	if b.bytes > 0 && b.buf.Len() > 0 {
		b.tc.t.Fatalf("can't interleave Write and writeBytes on request body")
	}
	if b.err != nil {
		b.tc.t.Fatalf("can't write to request body after closeWithError")
	}
}

// closeWithError sets an error which will be returned by Read.
func (b *testRequestBody) closeWithError(err error) {
	defer b.tc.sync()
	b.gate.lock()
	defer b.unlock()
	b.err = err
}

// roundTrip starts a RoundTrip call.
//
// (Note that the RoundTrip won't complete until response headers are received,
// the request times out, or some other terminal condition is reached.)
func (tc *testClientConn) roundTrip(req *http.Request) *testRoundTrip {
	rt := &testRoundTrip{
		t:     tc.t,
		donec: make(chan struct{}),
	}
	tc.roundtrips = append(tc.roundtrips, rt)
	go func() {
		tc.group.Join()
		defer close(rt.donec)
		rt.resp, rt.respErr = tc.cc.roundTrip(req, func(cs *clientStream) {
			rt.id.Store(cs.ID)
		})
	}()
	tc.sync()

	tc.t.Cleanup(func() {
		if !rt.done() {
			return
		}
		res, _ := rt.result()
		if res != nil {
			res.Body.Close()
		}
	})

	return rt
}

func (tc *testClientConn) greet(settings ...Setting) {
	tc.wantFrameType(FrameSettings)
	tc.wantFrameType(FrameWindowUpdate)
	tc.writeSettings(settings...)
	tc.writeSettingsAck()
	tc.wantFrameType(FrameSettings) // acknowledgement
}

// makeHeaderBlockFragment encodes headers in a form suitable for inclusion
// in a HEADERS or CONTINUATION frame.
//
// It takes a list of alernating names and values.
func (tc *testClientConn) makeHeaderBlockFragment(s ...string) []byte {
	if len(s)%2 != 0 {
		tc.t.Fatalf("uneven list of header name/value pairs")
	}
	tc.encbuf.Reset()
	for i := 0; i < len(s); i += 2 {
		tc.enc.WriteField(hpack.HeaderField{Name: s[i], Value: s[i+1]})
	}
	return tc.encbuf.Bytes()
}

// inflowWindow returns the amount of inbound flow control available for a stream,
// or for the connection if streamID is 0.
func (tc *testClientConn) inflowWindow(streamID uint32) int32 {
	tc.cc.mu.Lock()
	defer tc.cc.mu.Unlock()
	if streamID == 0 {
		return tc.cc.inflow.avail + tc.cc.inflow.unsent
	}
	cs := tc.cc.streams[streamID]
	if cs == nil {
		tc.t.Errorf("no stream with id %v", streamID)
		return -1
	}
	return cs.inflow.avail + cs.inflow.unsent
}

// testRoundTrip manages a RoundTrip in progress.
type testRoundTrip struct {
	t       *testing.T
	resp    *http.Response
	respErr error
	donec   chan struct{}
	id      atomic.Uint32
}

// streamID returns the HTTP/2 stream ID of the request.
func (rt *testRoundTrip) streamID() uint32 {
	id := rt.id.Load()
	if id == 0 {
		panic("stream ID unknown")
	}
	return id
}

// done reports whether RoundTrip has returned.
func (rt *testRoundTrip) done() bool {
	select {
	case <-rt.donec:
		return true
	default:
		return false
	}
}

// result returns the result of the RoundTrip.
func (rt *testRoundTrip) result() (*http.Response, error) {
	t := rt.t
	t.Helper()
	select {
	case <-rt.donec:
	default:
		t.Fatalf("RoundTrip is not done; want it to be")
	}
	return rt.resp, rt.respErr
}

// response returns the response of a successful RoundTrip.
// If the RoundTrip unexpectedly failed, it calls t.Fatal.
func (rt *testRoundTrip) response() *http.Response {
	t := rt.t
	t.Helper()
	resp, err := rt.result()
	if err != nil {
		t.Fatalf("RoundTrip returned unexpected error: %v", rt.respErr)
	}
	if resp == nil {
		t.Fatalf("RoundTrip returned nil *Response and nil error")
	}
	return resp
}

// err returns the (possibly nil) error result of RoundTrip.
func (rt *testRoundTrip) err() error {
	t := rt.t
	t.Helper()
	_, err := rt.result()
	return err
}

// wantStatus indicates the expected response StatusCode.
func (rt *testRoundTrip) wantStatus(want int) {
	t := rt.t
	t.Helper()
	if got := rt.response().StatusCode; got != want {
		t.Fatalf("got response status %v, want %v", got, want)
	}
}

// body reads the contents of the response body.
func (rt *testRoundTrip) readBody() ([]byte, error) {
	t := rt.t
	t.Helper()
	return io.ReadAll(rt.response().Body)
}

// wantBody indicates the expected response body.
// (Note that this consumes the body.)
func (rt *testRoundTrip) wantBody(want []byte) {
	t := rt.t
	t.Helper()
	got, err := rt.readBody()
	if err != nil {
		t.Fatalf("unexpected error reading response body: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("unexpected response body:\ngot:  %q\nwant: %q", got, want)
	}
}

// wantHeaders indicates the expected response headers.
func (rt *testRoundTrip) wantHeaders(want http.Header) {
	t := rt.t
	t.Helper()
	res := rt.response()
	if diff := diffHeaders(res.Header, want); diff != "" {
		t.Fatalf("unexpected response headers:\n%v", diff)
	}
}

// wantTrailers indicates the expected response trailers.
func (rt *testRoundTrip) wantTrailers(want http.Header) {
	t := rt.t
	t.Helper()
	res := rt.response()
	if diff := diffHeaders(res.Trailer, want); diff != "" {
		t.Fatalf("unexpected response trailers:\n%v", diff)
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

// A testTransport allows testing Transport.RoundTrip against fake servers.
// Tests that aren't specifically exercising RoundTrip's retry loop or connection pooling
// should use testClientConn instead.
type testTransport struct {
	t     *testing.T
	tr    *Transport
	group *synctestGroup

	ccs []*testClientConn
}

func newTestTransport(t *testing.T, opts ...any) *testTransport {
	tt := &testTransport{
		t:     t,
		group: newSynctest(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	tt.group.Join()

	tr := &Transport{}
	for _, o := range opts {
		switch o := o.(type) {
		case func(*http.Transport):
			if tr.t1 == nil {
				tr.t1 = &http.Transport{}
			}
			o(tr.t1)
		case func(*Transport):
			o(tr)
		case *Transport:
			tr = o
		}
	}
	tt.tr = tr

	tr.transportTestHooks = &transportTestHooks{
		group: tt.group,
		newclientconn: func(cc *ClientConn) {
			tc := newTestClientConnFromClientConn(t, cc)
			tt.ccs = append(tt.ccs, tc)
		},
	}

	t.Cleanup(func() {
		tt.sync()
		if len(tt.ccs) > 0 {
			t.Fatalf("%v test ClientConns created, but not examined by test", len(tt.ccs))
		}
		tt.group.Close(t)
	})

	return tt
}

func (tt *testTransport) sync() {
	tt.group.Wait()
}

func (tt *testTransport) advance(d time.Duration) {
	tt.group.AdvanceTime(d)
	tt.sync()
}

func (tt *testTransport) hasConn() bool {
	return len(tt.ccs) > 0
}

func (tt *testTransport) getConn() *testClientConn {
	tt.t.Helper()
	if len(tt.ccs) == 0 {
		tt.t.Fatalf("no new ClientConns created; wanted one")
	}
	tc := tt.ccs[0]
	tt.ccs = tt.ccs[1:]
	tc.sync()
	tc.readClientPreface()
	tc.sync()
	return tc
}

func (tt *testTransport) roundTrip(req *http.Request) *testRoundTrip {
	rt := &testRoundTrip{
		t:     tt.t,
		donec: make(chan struct{}),
	}
	go func() {
		tt.group.Join()
		defer close(rt.donec)
		rt.resp, rt.respErr = tt.tr.RoundTrip(req)
	}()
	tt.sync()

	tt.t.Cleanup(func() {
		if !rt.done() {
			return
		}
		res, _ := rt.result()
		if res != nil {
			res.Body.Close()
		}
	})

	return rt
}
