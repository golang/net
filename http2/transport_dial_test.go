// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The net/http wrapper requires the fix for go.dev/issue/81646 in Go 1.28.
//go:build !go1.27 || go1.28 || http2legacy

package http2_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"golang.org/x/net/http2"
)

// Concurrent calls to http2.Transport.RoundTrip share an in-progress dial,
// including when ConfigureTransports is used with an HTTP/1-capable transport.
func TestAPITransportCoalescesDials(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			if configured && !wrappedAPI {
				t.Skip("legacy ConfigureTransports installs a pool that does not dial")
			}
			synctest.Test(t, func(t *testing.T) {
				tt := newTestTransport(t, roundTripXNetHTTP2)
				if configured {
					var err error
					tt.tr, err = http2.ConfigureTransports(&http.Transport{})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					// Avoid newTestTransport's explicit HTTP/2-only Protocols.
					tt.tr = &http2.Transport{}
				}
				t.Cleanup(tt.tr.CloseIdleConnections)
				ready := make(chan struct{})
				var dials atomic.Int32
				tt.tr.DialTLSContext = func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
					dials.Add(1)
					<-ready
					return tt.li.newConn(), nil
				}
				var requests []*testRoundTrip
				for range 10 {
					req, _ := http.NewRequest("GET", "https://example.com/", nil)
					requests = append(requests, tt.roundTrip(req))
				}
				if got := dials.Load(); got != 1 {
					t.Errorf("got %v dials, want 1", got)
				}
				close(ready)
				tc := tt.getConn()
				tc.wantFrameType(http2.FrameSettings)
				tc.wantFrameType(http2.FrameWindowUpdate)
				tc.writeSettings()
				tc.writeSettingsAck()
				for range requests {
					f := readFrame[*http2.HeadersFrame](t, tc)
					tc.writeHeaders(http2.HeadersFrameParam{
						StreamID: f.StreamID, EndHeaders: true, EndStream: true,
						BlockFragment: tc.makeHeaderBlockFragment(":status", "200"),
					})
				}
				for _, rt := range requests {
					rt.wantStatus(200)
				}
			})
		})
	}
}

func TestAPITransportCoalescesOnlyHTTP2Dials(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ready := make(chan struct{})
		var dials atomic.Int32
		wantErr := errors.New("dial failed")
		tr := &http.Transport{
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				dials.Add(1)
				<-ready
				return nil, wantErr
			},
		}
		if _, err := http2.ConfigureTransports(tr); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(tr.CloseIdleConnections)
		done := make(chan error, 2)
		for range 2 {
			go func() {
				req, _ := http.NewRequest("GET", "https://example.com/", nil)
				_, err := tr.RoundTrip(req)
				done <- err
			}()
		}
		synctest.Wait()
		if got := dials.Load(); got != 2 {
			t.Errorf("got %v dials for an HTTP/1-capable transport, want 2", got)
		}
		close(ready)
		for range 2 {
			if err := <-done; !errors.Is(err, wantErr) {
				t.Errorf("RoundTrip error = %v, want %v", err, wantErr)
			}
		}
	})
}
