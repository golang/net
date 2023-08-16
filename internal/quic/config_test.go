// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import "testing"

func TestConfigTransportParameters(t *testing.T) {
	const (
		wantInitialMaxStreamData = int64(2)
	)
	tc := newTestConn(t, clientSide, func(c *Config) {
		c.StreamReadBufferSize = wantInitialMaxStreamData
	})
	tc.handshake()
	if tc.sentTransportParameters == nil {
		t.Fatalf("conn didn't send transport parameters during handshake")
	}
	p := tc.sentTransportParameters
	if got, want := p.initialMaxStreamDataBidiLocal, wantInitialMaxStreamData; got != want {
		t.Errorf("initial_max_stream_data_bidi_local = %v, want %v", got, want)
	}
	if got, want := p.initialMaxStreamDataBidiRemote, wantInitialMaxStreamData; got != want {
		t.Errorf("initial_max_stream_data_bidi_remote = %v, want %v", got, want)
	}
	if got, want := p.initialMaxStreamDataUni, wantInitialMaxStreamData; got != want {
		t.Errorf("initial_max_stream_data_uni = %v, want %v", got, want)
	}
}
