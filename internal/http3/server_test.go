// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.25

package http3

import (
	"io"
	"net/http"
	"net/netip"
	"testing"
	"testing/synctest"

	"golang.org/x/net/internal/quic/quicwire"
	"golang.org/x/net/quic"
)

func TestServerReceivePushStream(t *testing.T) {
	// "[...] if a server receives a client-initiated push stream,
	// this MUST be treated as a connection error of type H3_STREAM_CREATION_ERROR."
	// https://www.rfc-editor.org/rfc/rfc9114.html#section-6.2.2-3
	synctest.Test(t, func(t *testing.T) {
		ts := newTestServer(t, nil)
		tc := ts.connect()
		tc.newStream(streamTypePush)
		tc.wantClosed("invalid client-created push stream", errH3StreamCreationError)
	})
}

func TestServerCancelPushForUnsentPromise(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ts := newTestServer(t, nil)
		tc := ts.connect()
		tc.greet()

		const pushID = 100
		tc.control.writeVarint(int64(frameTypeCancelPush))
		tc.control.writeVarint(int64(quicwire.SizeVarint(pushID)))
		tc.control.writeVarint(pushID)
		tc.control.Flush()

		tc.wantClosed("client canceled never-sent push ID", errH3IDError)
	})
}

func TestServerHeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := w.Header()
			for key, values := range r.Header {
				for _, value := range values {
					header.Add(key, value)
				}
			}
			w.WriteHeader(204)
		}))
		tc := ts.connect()
		tc.greet()

		reqStream := tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(http.Header{
			"header-from-client": {"that", "should", "be", "echoed"},
		})
		synctest.Wait()
		reqStream.wantHeaders(map[string][]string{
			":status":            {"204"},
			"Header-From-Client": {"that", "should", "be", "echoed"},
		})
		reqStream.wantClosed("request is complete")
	})
}

func TestServerPseudoHeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Pseudo-headers from client request should populate a specific
			// field in http.Request, and should not be part of http.Request.Header.
			if r.Header.Get(":method") != "" || r.Method != "GET" {
				t.Error("want pseudo-headers from client to be reflected in appropriate fields in http.Request, not in http.Request.Header")
			}
			// Conversely, server should not be able to set pseudo-headers by
			// writing to the ResponseWriter's Header.
			header := w.Header()
			header.Add(":status", "123")
			w.WriteHeader(321)
		}))
		tc := ts.connect()
		tc.greet()

		reqStream := tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(http.Header{":method": {"GET"}})
		synctest.Wait()
		reqStream.wantHeaders(map[string][]string{":status": {"321"}})
		reqStream.wantClosed("request is complete")
	})
}

func TestServerInvalidHeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("valid-name", "valid value")
			// Invalid headers are skipped.
			w.Header().Add("invalid name with spaces", "some value")
			w.Header().Add("some-name", "invalid value with \n")
			w.Header().Add("valid-name-2", "valid value 2")
			w.WriteHeader(200)
		}))
		tc := ts.connect()
		tc.greet()

		reqStream := tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(http.Header{})
		synctest.Wait()
		reqStream.wantHeaders(map[string][]string{
			":status":      {"200"},
			"Valid-Name":   {"valid value"},
			"Valid-Name-2": {"valid value 2"},
		})
		reqStream.wantClosed("request is complete")
	})
}

func TestServerBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			w.Write([]byte(r.URL.Path)) // Implicitly calls w.WriteHeader(200).
			w.Write(body)
		}))
		tc := ts.connect()
		tc.greet()

		reqStream := tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(http.Header{
			":path": {"/"},
		})
		bodyContent := []byte("some body content that should be echoed")
		reqStream.writeData(bodyContent)
		reqStream.stream.stream.CloseWrite()
		synctest.Wait()
		reqStream.wantHeaders(http.Header{":status": {"200"}})
		reqStream.wantData([]byte("/"))
		reqStream.wantData(bodyContent)
		reqStream.wantClosed("request is complete")
	})
}

type testServer struct {
	t  testing.TB
	s  *Server
	tn testNet
	*testQUICEndpoint

	addr netip.AddrPort
}

type testQUICEndpoint struct {
	t testing.TB
	e *quic.Endpoint
}

type testServerConn struct {
	ts *testServer

	*testQUICConn
	control *testQUICStream
}

func newTestServer(t testing.TB, handler http.Handler) *testServer {
	t.Helper()
	ts := &testServer{
		t: t,
		s: &Server{
			Config: &quic.Config{
				TLSConfig: testTLSConfig,
			},
			Handler: handler,
		},
	}
	e := ts.tn.newQUICEndpoint(t, ts.s.Config)
	ts.addr = e.LocalAddr()
	go ts.s.Serve(e)
	return ts
}

func (ts *testServer) connect() *testServerConn {
	ts.t.Helper()
	config := &quic.Config{TLSConfig: testTLSConfig}
	e := ts.tn.newQUICEndpoint(ts.t, nil)
	qconn, err := e.Dial(ts.t.Context(), "udp", ts.addr.String(), config)
	if err != nil {
		ts.t.Fatal(err)
	}
	tc := &testServerConn{
		ts:           ts,
		testQUICConn: newTestQUICConn(ts.t, qconn),
	}
	synctest.Wait()
	return tc
}

// greet performs initial connection handshaking with the server.
func (tc *testServerConn) greet() {
	// Client creates a control stream.
	tc.control = tc.newStream(streamTypeControl)
	tc.control.writeVarint(int64(frameTypeSettings))
	tc.control.writeVarint(0) // size
	tc.control.Flush()
	synctest.Wait()
}
