// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.27

package http3_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	_ "unsafe" // for linkname

	"golang.org/x/net/internal/http3"
	"golang.org/x/net/internal/testcert"
)

//go:linkname protocolSetHTTP3
func protocolSetHTTP3(p *http.Protocols)

func newTestTLSConfig() *tls.Config {
	testCert := func() tls.Certificate {
		cert, err := tls.X509KeyPair(testcert.LocalhostCert, testcert.LocalhostKey)
		if err != nil {
			panic(err)
		}
		return cert
	}()
	config := &tls.Config{
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{testCert},
	}
	return config
}

func TestNetHTTPIntegration(t *testing.T) {
	body := []byte("some body")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})

	srv := &http.Server{
		Addr:      "127.0.0.1:0",
		Handler:   handler,
		TLSConfig: newTestTLSConfig(),
	}
	if err := http3.RegisterServer(srv, http3.ServerOpts{}); err != nil {
		t.Skipf("cannot register server: %v", err)
	}
	srv.Protocols = &http.Protocols{}
	protocolSetHTTP3(srv.Protocols)

	// We do not yet have a public API for serving on a system-chosen port
	// that lets us find out what that port is. (ListenAndServeTLS will listen on
	// a system-chosen port, but we can't find out what port it picked.)
	// So use ServeTLS with an pro tem mechanism for passing in a PacketConn.
	nc, err := net.ListenPacket("udp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	go srv.ServeTLS(http3ServeConn{nc}, "", "")

	tr := &http.Transport{TLSClientConfig: newTestTLSConfig()}
	tr.Protocols = &http.Protocols{}
	protocolSetHTTP3(tr.Protocols)
	if err := http3.RegisterTransport(tr, http3.TransportOpts{}); err != nil {
		// If RegisterServer above succeeded, this should as well.
		t.Fatalf("cannot register transport: %v", err)
	}

	client := &http.Client{
		Transport: tr,
		// Be extra generous with the timeout, to account for smaller builders
		// that we use for e.g. plan9.
		Timeout: 5 * time.Second,
	}

	for range 5 {
		req, err := http.NewRequest("GET", "https://"+nc.LocalAddr().String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(b, body) {
			t.Errorf("got %v, want %v", string(b), string(body))
		}
		// TestMain checks that there are no leaked goroutines after tests have
		// finished running.
		// Over here, we verify that closing the idle connections of a net/http
		// Transport will result in HTTP/3 transport closing any UDP sockets
		// after there are no longer any open connections.
		// We do this in a loop to verify that CloseIdleConnections will not
		// prevent transport from creating a new connection should a new dial
		// be started.
		tr.CloseIdleConnections()
	}
	// Similarly when a net/http Server shuts down, the HTTP/3 server should
	// also follow.
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

type http3ServeConn struct {
	conn net.PacketConn
}

func (c http3ServeConn) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (c http3ServeConn) Close() error              { return nil }
func (c http3ServeConn) Addr() net.Addr            { return nil }

func (c http3ServeConn) HTTP3PacketConn() net.PacketConn {
	return c.conn
}
