// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.24 && goexperiment.synctest

package http3

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"
	"testing/synctest"

	"golang.org/x/net/quic"
)

func TestRoundTripSimple(t *testing.T) {
	runSynctest(t, func(t testing.TB) {
		tc := newTestClientConn(t)
		tc.greet()

		req, _ := http.NewRequest("GET", "https://example.tld/", nil)
		req.Header["User-Agent"] = nil
		rt := tc.roundTrip(req)
		st := tc.wantStream(streamTypeRequest)
		st.wantHeaders(http.Header{
			":authority": []string{"example.tld"},
			":method":    []string{"GET"},
			":path":      []string{"/"},
			":scheme":    []string{"https"},
		})
		st.writeHeaders(http.Header{
			":status":       []string{"200"},
			"x-some-header": []string{"value"},
		})
		rt.wantStatus(200)
		rt.wantHeaders(http.Header{
			"X-Some-Header": []string{"value"},
		})
	})
}

func TestRoundTripWithBadHeaders(t *testing.T) {
	runSynctest(t, func(t testing.TB) {
		tc := newTestClientConn(t)
		tc.greet()

		req, _ := http.NewRequest("GET", "https://example.tld/", nil)
		req.Header["Invalid\nHeader"] = []string{"x"}
		rt := tc.roundTrip(req)
		rt.wantError("RoundTrip fails when request contains invalid headers")
	})
}

func TestRoundTripWithUnknownFrame(t *testing.T) {
	runSynctest(t, func(t testing.TB) {
		tc := newTestClientConn(t)
		tc.greet()

		req, _ := http.NewRequest("GET", "https://example.tld/", nil)
		rt := tc.roundTrip(req)
		st := tc.wantStream(streamTypeRequest)
		st.wantHeaders(nil)

		// Write an unknown frame type before the response HEADERS.
		data := "frame content"
		st.writeVarint(0x1f + 0x21)      // reserved frame type
		st.writeVarint(int64(len(data))) // size
		st.Write([]byte(data))

		st.writeHeaders(http.Header{
			":status": []string{"200"},
		})
		rt.wantStatus(200)
	})
}

func TestRoundTripWithInvalidPushPromise(t *testing.T) {
	// "A client MUST treat receipt of a PUSH_PROMISE frame that contains
	// a larger push ID than the client has advertised as a connection error of H3_ID_ERROR."
	// https://www.rfc-editor.org/rfc/rfc9114.html#section-7.2.5-5
	runSynctest(t, func(t testing.TB) {
		tc := newTestClientConn(t)
		tc.greet()

		req, _ := http.NewRequest("GET", "https://example.tld/", nil)
		rt := tc.roundTrip(req)
		st := tc.wantStream(streamTypeRequest)
		st.wantHeaders(nil)

		// Write a PUSH_PROMISE frame.
		// Since the client hasn't indicated willingness to accept pushes,
		// this is a connection error.
		st.writePushPromise(0, http.Header{
			":path": []string{"/foo"},
		})
		rt.wantError("RoundTrip fails after receiving invalid PUSH_PROMISE")
		tc.wantClosed(
			"push ID exceeds client's MAX_PUSH_ID",
			errH3IDError,
		)
	})
}

func TestRoundTripResponseContentLength(t *testing.T) {
	for _, test := range []struct {
		name              string
		respHeader        http.Header
		wantContentLength int64
		wantError         bool
	}{{
		name: "valid",
		respHeader: http.Header{
			":status":        []string{"200"},
			"content-length": []string{"100"},
		},
		wantContentLength: 100,
	}, {
		name: "absent",
		respHeader: http.Header{
			":status": []string{"200"},
		},
		wantContentLength: -1,
	}, {
		name: "unparseable",
		respHeader: http.Header{
			":status":        []string{"200"},
			"content-length": []string{"1 1"},
		},
		wantError: true,
	}, {
		name: "duplicated",
		respHeader: http.Header{
			":status":        []string{"200"},
			"content-length": []string{"500", "500", "500"},
		},
		wantContentLength: 500,
	}, {
		name: "inconsistent",
		respHeader: http.Header{
			":status":        []string{"200"},
			"content-length": []string{"1", "2"},
		},
		wantError: true,
	}, {
		// 204 responses aren't allowed to contain a Content-Length header.
		// We just ignore it.
		name: "204",
		respHeader: http.Header{
			":status":        []string{"204"},
			"content-length": []string{"100"},
		},
		wantContentLength: -1,
	}} {
		runSynctestSubtest(t, test.name, func(t testing.TB) {
			tc := newTestClientConn(t)
			tc.greet()

			req, _ := http.NewRequest("GET", "https://example.tld/", nil)
			rt := tc.roundTrip(req)
			st := tc.wantStream(streamTypeRequest)
			st.wantHeaders(nil)
			st.writeHeaders(test.respHeader)
			if test.wantError {
				rt.wantError("invalid content-length in response")
				return
			}
			if got, want := rt.response().ContentLength, test.wantContentLength; got != want {
				t.Errorf("Response.ContentLength = %v, want %v", got, want)
			}
		})
	}
}

func TestRoundTripMalformedResponses(t *testing.T) {
	for _, test := range []struct {
		name       string
		respHeader http.Header
	}{{
		name: "duplicate :status",
		respHeader: http.Header{
			":status": []string{"200", "204"},
		},
	}, {
		name: "unparseable :status",
		respHeader: http.Header{
			":status": []string{"frogpants"},
		},
	}, {
		name: "undefined pseudo-header",
		respHeader: http.Header{
			":status":  []string{"200"},
			":unknown": []string{"x"},
		},
	}, {
		name:       "no :status",
		respHeader: http.Header{},
	}} {
		runSynctestSubtest(t, test.name, func(t testing.TB) {
			tc := newTestClientConn(t)
			tc.greet()

			req, _ := http.NewRequest("GET", "https://example.tld/", nil)
			rt := tc.roundTrip(req)
			st := tc.wantStream(streamTypeRequest)
			st.wantHeaders(nil)
			st.writeHeaders(test.respHeader)
			rt.wantError("malformed response")
		})
	}
}

func TestRoundTripCrumbledCookiesInResponse(t *testing.T) {
	// "If a decompressed field section contains multiple cookie field lines,
	// these MUST be concatenated into a single byte string [...]"
	// using the two-byte delimiter of "; "''
	// https://www.rfc-editor.org/rfc/rfc9114.html#section-4.2.1-2
	runSynctest(t, func(t testing.TB) {
		tc := newTestClientConn(t)
		tc.greet()

		req, _ := http.NewRequest("GET", "https://example.tld/", nil)
		rt := tc.roundTrip(req)
		st := tc.wantStream(streamTypeRequest)
		st.wantHeaders(nil)
		st.writeHeaders(http.Header{
			":status": []string{"200"},
			"cookie":  []string{"a=1", "b=2; c=3", "d=4"},
		})
		rt.wantStatus(200)
		rt.wantHeaders(http.Header{
			"Cookie": []string{"a=1; b=2; c=3; d=4"},
		})
	})
}

func TestRoundTripResponseBody(t *testing.T) {
	// These tests consist of a series of steps,
	// where each step is either something arriving on the response stream
	// or the client reading from the request body.
	type (
		// HEADERS frame arrives on the response stream (headers or trailers).
		receiveHeaders http.Header
		// DATA frame header arrives on the response stream.
		receiveDataHeader struct {
			size int64
		}
		// DATA frame content arrives on the response stream.
		receiveData struct {
			size int64
		}
		// Some other frame arrives on the response stream.
		receiveFrame struct {
			ftype frameType
			data  []byte
		}
		// Response stream closed, ending the body.
		receiveEOF struct{}
		// Client reads from Response.Body.
		wantBody struct {
			size int64
			eof  bool
		}
		wantError struct{}
	)
	for _, test := range []struct {
		name       string
		respHeader http.Header
		steps      []any
		wantError  bool
	}{{
		name: "no content length",
		steps: []any{
			receiveHeaders{
				":status": []string{"200"},
			},
			receiveDataHeader{size: 10},
			receiveData{size: 10},
			receiveEOF{},
			wantBody{size: 10, eof: true},
		},
	}, {
		name: "valid content length",
		steps: []any{
			receiveHeaders{
				":status":        []string{"200"},
				"content-length": []string{"10"},
			},
			receiveDataHeader{size: 10},
			receiveData{size: 10},
			receiveEOF{},
			wantBody{size: 10, eof: true},
		},
	}, {
		name: "data frame exceeds content length",
		steps: []any{
			receiveHeaders{
				":status":        []string{"200"},
				"content-length": []string{"5"},
			},
			receiveDataHeader{size: 10},
			receiveData{size: 10},
			wantError{},
		},
	}, {
		name: "data frame after all content read",
		steps: []any{
			receiveHeaders{
				":status":        []string{"200"},
				"content-length": []string{"5"},
			},
			receiveDataHeader{size: 5},
			receiveData{size: 5},
			wantBody{size: 5},
			receiveDataHeader{size: 1},
			receiveData{size: 1},
			wantError{},
		},
	}, {
		name: "content length too long",
		steps: []any{
			receiveHeaders{
				":status":        []string{"200"},
				"content-length": []string{"10"},
			},
			receiveDataHeader{size: 5},
			receiveData{size: 5},
			receiveEOF{},
			wantBody{size: 5},
			wantError{},
		},
	}, {
		name: "stream ended by trailers",
		steps: []any{
			receiveHeaders{
				":status": []string{"200"},
			},
			receiveDataHeader{size: 5},
			receiveData{size: 5},
			receiveHeaders{
				"x-trailer": []string{"value"},
			},
			wantBody{size: 5, eof: true},
		},
	}, {
		name: "trailers and content length too long",
		steps: []any{
			receiveHeaders{
				":status":        []string{"200"},
				"content-length": []string{"10"},
			},
			receiveDataHeader{size: 5},
			receiveData{size: 5},
			wantBody{size: 5},
			receiveHeaders{
				"x-trailer": []string{"value"},
			},
			wantError{},
		},
	}, {
		name: "unknown frame before headers",
		steps: []any{
			receiveFrame{
				ftype: 0x1f + 0x21, // reserved frame type
				data:  []byte{1, 2, 3, 4},
			},
			receiveHeaders{
				":status": []string{"200"},
			},
			receiveDataHeader{size: 10},
			receiveData{size: 10},
			wantBody{size: 10},
		},
	}, {
		name: "unknown frame after headers",
		steps: []any{
			receiveHeaders{
				":status": []string{"200"},
			},
			receiveFrame{
				ftype: 0x1f + 0x21, // reserved frame type
				data:  []byte{1, 2, 3, 4},
			},
			receiveDataHeader{size: 10},
			receiveData{size: 10},
			wantBody{size: 10},
		},
	}, {
		name: "invalid frame",
		steps: []any{
			receiveHeaders{
				":status": []string{"200"},
			},
			receiveFrame{
				ftype: frameTypeSettings, // not a valid frame on this stream
				data:  []byte{1, 2, 3, 4},
			},
			wantError{},
		},
	}, {
		name: "data frame consumed by several reads",
		steps: []any{
			receiveHeaders{
				":status": []string{"200"},
			},
			receiveDataHeader{size: 16},
			receiveData{size: 16},
			wantBody{size: 2},
			wantBody{size: 4},
			wantBody{size: 8},
			wantBody{size: 2},
		},
	}, {
		name: "read multiple frames",
		steps: []any{
			receiveHeaders{
				":status": []string{"200"},
			},
			receiveDataHeader{size: 2},
			receiveData{size: 2},
			receiveDataHeader{size: 4},
			receiveData{size: 4},
			receiveDataHeader{size: 8},
			receiveData{size: 8},
			wantBody{size: 2},
			wantBody{size: 4},
			wantBody{size: 8},
		},
	}} {
		runSynctestSubtest(t, test.name, func(t testing.TB) {
			tc := newTestClientConn(t)
			tc.greet()

			req, _ := http.NewRequest("GET", "https://example.tld/", nil)
			rt := tc.roundTrip(req)
			st := tc.wantStream(streamTypeRequest)
			st.wantHeaders(nil)

			var (
				bytesSent     int
				bytesReceived int
			)
			for _, step := range test.steps {
				switch step := step.(type) {
				case receiveHeaders:
					st.writeHeaders(http.Header(step))
				case receiveDataHeader:
					t.Logf("receive DATA frame header: size=%v", step.size)
					st.writeVarint(int64(frameTypeData))
					st.writeVarint(step.size)
					st.Flush()
				case receiveData:
					t.Logf("receive DATA frame content: size=%v", step.size)
					for range step.size {
						st.stream.stream.WriteByte(byte(bytesSent))
						bytesSent++
					}
					st.Flush()
				case receiveFrame:
					st.writeVarint(int64(step.ftype))
					st.writeVarint(int64(len(step.data)))
					st.Write(step.data)
					st.Flush()
				case receiveEOF:
					t.Logf("receive EOF on request stream")
					st.stream.stream.CloseWrite()
				case wantBody:
					t.Logf("read %v bytes from response body", step.size)
					want := make([]byte, step.size)
					for i := range want {
						want[i] = byte(bytesReceived)
						bytesReceived++
					}
					got := make([]byte, step.size)
					n, err := rt.response().Body.Read(got)
					got = got[:n]
					if !bytes.Equal(got, want) {
						t.Errorf("resp.Body.Read:")
						t.Errorf("  got:  {%x}", got)
						t.Fatalf("  want: {%x}", want)
					}
					if err != nil {
						if step.eof && err == io.EOF {
							continue
						}
						t.Fatalf("resp.Body.Read: unexpected error %v", err)
					}
					if step.eof {
						if n, err := rt.response().Body.Read([]byte{0}); n != 0 || err != io.EOF {
							t.Fatalf("resp.Body.Read() = %v, %v; want io.EOF", n, err)
						}
					}
				case wantError:
					if n, err := rt.response().Body.Read([]byte{0}); n != 0 || err == nil || err == io.EOF {
						t.Fatalf("resp.Body.Read() = %v, %v; want error", n, err)
					}
				default:
					t.Fatalf("unknown test step %T", step)
				}
			}
		})
	}
}

func TestRoundTripRequestBodySent(t *testing.T) {
	runSynctest(t, func(t testing.TB) {
		tc := newTestClientConn(t)
		tc.greet()

		bodyr, bodyw := io.Pipe()

		req, _ := http.NewRequest("GET", "https://example.tld/", bodyr)
		rt := tc.roundTrip(req)
		st := tc.wantStream(streamTypeRequest)
		st.wantHeaders(nil)

		bodyw.Write([]byte{0, 1, 2, 3, 4})
		st.wantData([]byte{0, 1, 2, 3, 4})

		bodyw.Write([]byte{5, 6, 7})
		st.wantData([]byte{5, 6, 7})

		bodyw.Close()
		st.wantClosed("request body sent")

		st.writeHeaders(http.Header{
			":status": []string{"200"},
		})
		rt.wantStatus(200)
		rt.response().Body.Close()
	})
}

func TestRoundTripRequestBodyErrors(t *testing.T) {
	for _, test := range []struct {
		name          string
		body          io.Reader
		contentLength int64
	}{{
		name:          "too short",
		contentLength: 10,
		body:          bytes.NewReader([]byte{0, 1, 2, 3, 4}),
	}, {
		name:          "too long",
		contentLength: 5,
		body:          bytes.NewReader([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}),
	}, {
		name: "read error",
		body: io.MultiReader(
			bytes.NewReader([]byte{0, 1, 2, 3, 4}),
			&testReader{
				readFunc: func([]byte) (int, error) {
					return 0, errors.New("read error")
				},
			},
		),
	}} {
		runSynctestSubtest(t, test.name, func(t testing.TB) {
			tc := newTestClientConn(t)
			tc.greet()

			req, _ := http.NewRequest("GET", "https://example.tld/", test.body)
			req.ContentLength = test.contentLength
			rt := tc.roundTrip(req)
			st := tc.wantStream(streamTypeRequest)

			// The Transport should send some number of frames before detecting an
			// error in the request body and aborting the request.
			synctest.Wait()
			for {
				_, err := st.readFrameHeader()
				if err != nil {
					var code quic.StreamErrorCode
					if !errors.As(err, &code) {
						t.Fatalf("request stream closed with error %v: want QUIC stream error", err)
					}
					break
				}
				if err := st.discardFrame(); err != nil {
					t.Fatalf("discardFrame: %v", err)
				}
			}

			// RoundTrip returns with an error.
			rt.wantError("request fails due to body error")
		})
	}
}

func TestRoundTripRequestBodyErrorAfterHeaders(t *testing.T) {
	runSynctest(t, func(t testing.TB) {
		tc := newTestClientConn(t)
		tc.greet()

		bodyr, bodyw := io.Pipe()
		req, _ := http.NewRequest("GET", "https://example.tld/", bodyr)
		req.ContentLength = 10
		rt := tc.roundTrip(req)
		st := tc.wantStream(streamTypeRequest)

		// Server sends response headers, and RoundTrip returns.
		// The request body hasn't been sent yet.
		st.wantHeaders(nil)
		st.writeHeaders(http.Header{
			":status": []string{"200"},
		})
		rt.wantStatus(200)

		// Write too many bytes to the request body, triggering a request error.
		bodyw.Write(make([]byte, req.ContentLength+1))

		//io.Copy(io.Discard, st)
		st.wantError(quic.StreamErrorCode(errH3InternalError))

		if err := rt.response().Body.Close(); err == nil {
			t.Fatalf("Response.Body.Close() = %v, want error", err)
		}
	})
}
