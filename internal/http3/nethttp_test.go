// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.27

package http3_test

import (
	"crypto/tls"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	_ "unsafe" // for linkname

	"golang.org/x/net/internal/http3"
	"golang.org/x/net/internal/testcert"
	"golang.org/x/net/quic"
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
	srv.Protocols = &http.Protocols{}
	protocolSetHTTP3(srv.Protocols)

	var listenAddr string
	listenAddrSet := make(chan any)
	http3.RegisterServer(srv, http3.ServerOpts{
		ListenQUIC: func(addr string, config *quic.Config) (*quic.Endpoint, error) {
			e, err := quic.Listen("udp", addr, config)
			listenAddr = e.LocalAddr().String()
			listenAddrSet <- struct{}{}
			return e, err
		},
	})
	go func() {
		if err := srv.ListenAndServeTLS("", ""); err != nil {
			panic(err)
		}
	}()

	tr := &http.Transport{TLSClientConfig: newTestTLSConfig()}
	tr.Protocols = &http.Protocols{}
	protocolSetHTTP3(tr.Protocols)
	http3.RegisterTransport(tr)

	client := &http.Client{
		Transport: tr,
		Timeout:   time.Second,
	}
	<-listenAddrSet
	req, err := http.NewRequest("GET", "https://"+listenAddr, nil)
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
}
