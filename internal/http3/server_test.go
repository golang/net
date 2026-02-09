// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http3

import (
	"io"
	"maps"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/net/internal/quic/quicwire"
	"golang.org/x/net/quic"
)

// requestHeader is a helper function to make sure that all required
// pseudo-headers exist in an http.Header used for a request. Per
// https://www.rfc-editor.org/rfc/rfc9114.html#name-request-pseudo-header-field:
// "All HTTP/3 requests MUST include exactly one value for the :method,
// :scheme, and :path pseudo-header fields, unless the request is a CONNECT
// request;"
func requestHeader(h http.Header) http.Header {
	minimalHeader := http.Header{
		":method": {"GET"},
		":scheme": {"https"},
		":path":   {"/"},
	}
	maps.Copy(minimalHeader, h)
	return minimalHeader
}

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
		reqStream.writeHeaders(requestHeader(http.Header{
			"header-from-client": {"that", "should", "be", "echoed"},
		}))
		synctest.Wait()
		reqStream.wantHeaders(http.Header{
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
			if len(r.Header) != 0 {
				t.Errorf("got %v, want request header to be empty", r.Header)
			}
			if r.Method != "GET" {
				t.Errorf("got %v, want GET method", r.Method)
			}
			if r.Host != "fake.tld:1234" {
				t.Errorf("got %v, want fake.tld:1234", r.Host)
			}
			wantURL := &url.URL{
				Path:     "/some/path",
				RawQuery: "query=value&query2=value2#fragment",
			}
			if !reflect.DeepEqual(r.URL, wantURL) {
				t.Errorf("got %v, want URL to be %v", r.URL, wantURL)
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
		reqStream.writeHeaders(http.Header{
			":method":    {"GET"},
			":authority": {"fake.tld:1234"},
			":scheme":    {"https"},
			":path":      {"/some/path?query=value&query2=value2#fragment"},
		})
		synctest.Wait()
		reqStream.wantHeaders(http.Header{":status": {"321"}})
		reqStream.wantClosed("request is complete")

		reqStream = tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(http.Header{}) // Missing pseudo-header.
		synctest.Wait()
		reqStream.wantError(quic.StreamErrorCode(errH3MessageError))
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
		reqStream.writeHeaders(requestHeader(nil))
		synctest.Wait()
		reqStream.wantHeaders(http.Header{
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
		reqStream.writeHeaders(requestHeader(nil))
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

func TestServerHeadResponseNoBody(t *testing.T) {
	bodyContent := []byte("response body that will not be sent for HEAD requests")
	synctest.Test(t, func(t *testing.T) {
		ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(bodyContent)
		}))
		tc := ts.connect()
		tc.greet()

		reqStream := tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(requestHeader(nil))
		synctest.Wait()
		reqStream.wantHeaders(http.Header{":status": {"200"}})
		reqStream.wantData(bodyContent)
		reqStream.wantClosed("request is complete")

		reqStream = tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(requestHeader(http.Header{":method": {http.MethodHead}}))
		synctest.Wait()
		reqStream.wantHeaders(http.Header{":status": {"200"}})
		reqStream.wantClosed("request is complete")
	})
}

func TestServerHandlerEmpty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Empty handler should return a 200 OK
		}))
		tc := ts.connect()
		tc.greet()

		reqStream := tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(requestHeader(nil))
		synctest.Wait()
		reqStream.wantHeaders(http.Header{":status": {"200"}})
		reqStream.wantClosed("request is complete")
	})
}

func TestServerHandlerFlushing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(time.Second)
			w.Write([]byte("first"))

			time.Sleep(time.Second)
			w.Write([]byte("second"))
			w.(http.Flusher).Flush()

			time.Sleep(time.Second)
			w.Write([]byte("third"))
		}))
		tc := ts.connect()
		tc.greet()

		reqStream := tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(requestHeader(nil))
		synctest.Wait()

		respBody := make([]byte, 100)

		time.Sleep(time.Second)
		synctest.Wait()
		if n, err := reqStream.Read(respBody); err == nil {
			t.Errorf("got %v bytes read, want no message yet", n)
		}

		time.Sleep(time.Second)
		synctest.Wait()
		if _, err := reqStream.Read(respBody); err != nil {
			t.Errorf("failed to read partial response from server, got err: %v", err)
		}

		time.Sleep(time.Second)
		synctest.Wait()
		if _, err := reqStream.Read(respBody); err != io.EOF {
			t.Errorf("got err %v, want EOF", err)
		}
		reqStream.wantClosed("request is complete")
	})
}

func TestServerHandlerStreaming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stream := make(chan string)
		ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Flushing when we have not written anything yet implicitly calls
			// w.WriteHeader(200).
			w.(http.Flusher).Flush()
			for str := range stream {
				w.Write([]byte(str))
				w.(http.Flusher).Flush()
			}
		}))
		tc := ts.connect()
		tc.greet()

		reqStream := tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(requestHeader(nil))
		synctest.Wait()
		reqStream.wantHeaders(http.Header{":status": {"200"}})

		for _, data := range []string{"a", "bunch", "of", "things", "to", "stream"} {
			stream <- data
			reqStream.wantData([]byte(data))
		}
		close(stream)
		reqStream.wantClosed("request is complete")
	})
}

func TestServerExpect100Continue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		streamIdle := make(chan bool)
		ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Expect: 100-continue header should not be accessible from the
			// server handler.
			if len(r.Header) > 0 {
				t.Errorf("got %v, want request header to be empty", r.Header)
			}
			// Reading the body will cause the server to call w.WriteHeader(100).
			<-streamIdle
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			// Implicitly calls w.WriteHeader(200) since non-1XX status code
			// has been sent yet so far.
			w.Write(body)
		}))
		tc := ts.connect()
		tc.greet()

		// Client sends an Expect: 100-continue request.
		reqStream := tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(requestHeader(http.Header{
			"Expect": {"100-continue"},
		}))

		reqStream.wantIdle("stream is idle until server sends an HTTP 100 status")
		streamIdle <- true
		// Wait until server responds with HTTP status 100 before sending the
		// body.
		synctest.Wait()
		reqStream.wantHeaders(http.Header{":status": {"100"}})
		body := []byte("body that will be echoed back if we get status 100")
		reqStream.writeData(body)
		reqStream.stream.stream.CloseWrite()

		// Receive the server's response after sending the body.
		reqStream.wantHeaders(http.Header{":status": {"200"}})
		reqStream.wantData(body)
		reqStream.wantClosed("request is complete")
	})
}

func TestServerExpect100ContinueRejected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rejectBody := []byte("not allowed")
		ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(403)
			w.Write(rejectBody)
		}))
		tc := ts.connect()
		tc.greet()

		// Client sends an Expect: 100-continue request.
		reqStream := tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(requestHeader(http.Header{
			"Expect": {"100-continue"},
		}))

		// Server rejects it.
		synctest.Wait()
		reqStream.wantHeaders(http.Header{":status": {"403"}})
		reqStream.wantData(rejectBody)
		reqStream.wantClosed("request is complete")
	})
}

func TestServerHandlerReadReqWithNoBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serverBody := []byte("hello from server!")
		ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.ReadAll(r.Body); err != nil {
				t.Errorf("got %v err when reading from an empty request body, want nil", err)
			}
			w.Write(serverBody)
		}))
		tc := ts.connect()
		tc.greet()

		// Case 1: we know that there is no body / DATA frame because the
		// client closes the write direction of the stream.
		reqStream := tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(requestHeader(nil))
		reqStream.stream.stream.CloseWrite()
		synctest.Wait()
		reqStream.wantHeaders(http.Header{":status": {"200"}})
		reqStream.wantData(serverBody)
		reqStream.wantClosed("request is complete")

		// Case 2: we know that there is no body / DATA frame because the
		// client indicates a Content-Length of 0.
		reqStream = tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(requestHeader(http.Header{
			"Content-Length": {"0"},
		}))
		synctest.Wait()
		reqStream.wantHeaders(http.Header{":status": {"200"}})
		reqStream.wantData(serverBody)
		reqStream.wantClosed("request is complete")
	})
}

func TestServerHandlerReadTrailer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body := []byte("some body")
		ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wantTrailer := http.Header{
				"Client-Trailer-A": nil,
				"Client-Trailer-B": nil,
			}
			if !reflect.DeepEqual(r.Trailer, wantTrailer) {
				t.Errorf("got %v; want trailer to be %v before reading the body", r.Trailer, wantTrailer)
			}
			if _, err := io.ReadAll(r.Body); err != nil {
				t.Fatal(err)
			}
			wantTrailer = http.Header{
				"Client-Trailer-A": {"valuea"},
				"Client-Trailer-B": {"valueb"},
			}
			if !reflect.DeepEqual(r.Trailer, wantTrailer) {
				t.Errorf("got %v; want trailer to be %v after reading the body", r.Trailer, wantTrailer)
			}
			w.WriteHeader(200)
		}))
		tc := ts.connect()
		tc.greet()

		reqStream := tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(requestHeader(http.Header{
			"Trailer": {"Client-Trailer-A, Client-Trailer-B"},
		}))
		reqStream.writeData(body)
		reqStream.writeHeaders(http.Header{
			"Client-Trailer-A":   {"valuea"},
			"Client-Trailer-B":   {"valueb"},
			"Undeclared-Trailer": {"undeclared"}, // Undeclared trailer should be ignored.
		})
		synctest.Wait()
		reqStream.wantHeaders(nil)
		reqStream.wantClosed("request is complete")
	})
}

func TestServerHandlerReadTrailerNoBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wantTrailer := http.Header{
				"Client-Trailer-A": nil,
				"Client-Trailer-B": nil,
			}
			if !reflect.DeepEqual(r.Trailer, wantTrailer) {
				t.Errorf("got %v; want trailer to be %v before reading the body", r.Trailer, wantTrailer)
			}
			if _, err := io.ReadAll(r.Body); err != nil {
				t.Fatal(err)
			}
			wantTrailer = http.Header{
				"Client-Trailer-A": {"valuea"},
				"Client-Trailer-B": {"valueb"},
			}
			if !reflect.DeepEqual(r.Trailer, wantTrailer) {
				t.Errorf("got %v; want trailer to be %v after reading the body", r.Trailer, wantTrailer)
			}
			w.WriteHeader(200)
		}))
		tc := ts.connect()
		tc.greet()

		reqStream := tc.newStream(streamTypeRequest)
		reqStream.writeHeaders(requestHeader(http.Header{
			"Trailer":        {"Client-Trailer-A, Client-Trailer-B"},
			"Content-Length": {"0"},
		}))
		reqStream.writeHeaders(http.Header{
			"Client-Trailer-A":   {"valuea"},
			"Client-Trailer-B":   {"valueb"},
			"Undeclared-Trailer": {"undeclared"}, // Undeclared trailer should be ignored.
		})
		synctest.Wait()
		reqStream.wantHeaders(nil)
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
