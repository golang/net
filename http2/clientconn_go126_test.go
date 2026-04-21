// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.26

package http2_test

import (
	"net/http"
	"testing"
)

type httpClientConn = http.ClientConn

func newTestClientConn(t testing.TB, opts ...any) *testClientConn {
	t.Helper()

	tt := newTestTransport(t, opts...)

	switch tt.mode {
	case roundTripNetHTTP:
		cc, err := tt.tr1.NewClientConn(t.Context(), "http", "localhost:80")
		if err != nil {
			t.Fatalf("NewClientConn: %v", err)
		}

		tc := tt.getConn()
		tc.cc1 = cc
		return tc
	case roundTripXNetHTTP2:
		nc := tt.li.newConn()
		const singleUse = false
		_, err := tt.tr.TestNewClientConn(nc, singleUse, nil)
		if err != nil {
			t.Fatalf("newClientConn: %v", err)
		}

		return tt.getConn()
	default:
		panic("unknown test mode")
	}
}
