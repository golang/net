// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import "net/http"

// To configure an [http.Transport] to use HTTP/2,
// set the [http.Transport.Protocols] field.
func ExampleConfigureTransport() {
	tr := &http.Transport{}
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetHTTP1(true) // enable HTTP/1
	tr.Protocols.SetHTTP2(true) // enable HTTP/2
}

// To configure an [http.Transport] to use HTTP/2,
// set the [http.Transport.Protocols] field.
func ExampleConfigureTransports() {
	tr := &http.Transport{}
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetHTTP1(true) // enable HTTP/1
	tr.Protocols.SetHTTP2(true) // enable HTTP/2
}

// To configure an [http.Server] to use HTTP/2,
// set the [http.Server.Protocols] field.
func ExampleConfigureServer() {
	server := &http.Server{}
	server.Protocols = new(http.Protocols)
	server.Protocols.SetHTTP1(true) // enable HTTP/1
	server.Protocols.SetHTTP2(true) // enable HTTP/2
}
