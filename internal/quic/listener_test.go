// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func TestConnect(t *testing.T) {
	newLocalConnPair(t, &Config{}, &Config{})
}

func TestStreamTransfer(t *testing.T) {
	ctx := context.Background()
	cli, srv := newLocalConnPair(t, &Config{}, &Config{})
	data := makeTestData(1 << 20)

	srvdone := make(chan struct{})
	go func() {
		defer close(srvdone)
		s, err := srv.AcceptStream(ctx)
		if err != nil {
			t.Errorf("AcceptStream: %v", err)
			return
		}
		b, err := io.ReadAll(s)
		if err != nil {
			t.Errorf("io.ReadAll(s): %v", err)
			return
		}
		if !bytes.Equal(b, data) {
			t.Errorf("read data mismatch (got %v bytes, want %v", len(b), len(data))
		}
		if err := s.Close(); err != nil {
			t.Errorf("s.Close() = %v", err)
		}
	}()

	s, err := cli.NewStream(ctx)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	n, err := io.Copy(s, bytes.NewBuffer(data))
	if n != int64(len(data)) || err != nil {
		t.Fatalf("io.Copy(s, data) = %v, %v; want %v, nil", n, err, len(data))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("s.Close() = %v", err)
	}
}

func newLocalConnPair(t *testing.T, conf1, conf2 *Config) (clientConn, serverConn *Conn) {
	t.Helper()
	ctx := context.Background()
	l1 := newLocalListener(t, serverSide, conf1)
	l2 := newLocalListener(t, clientSide, conf2)
	c2, err := l2.Dial(ctx, "udp", l1.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	c1, err := l1.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return c2, c1
}

func newLocalListener(t *testing.T, side connSide, conf *Config) *Listener {
	t.Helper()
	if conf.TLSConfig == nil {
		conf.TLSConfig = newTestTLSConfig(side)
	}
	l, err := Listen("udp", "127.0.0.1:0", conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		l.Close(context.Background())
	})
	return l
}
