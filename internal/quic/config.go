// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"crypto/tls"
)

// A Config structure configures a QUIC endpoint.
// A Config must not be modified after it has been passed to a QUIC function.
// A Config may be reused; the quic package will also not modify it.
type Config struct {
	// TLSConfig is the endpoint's TLS configuration.
	// It must be non-nil and include at least one certificate or else set GetCertificate.
	TLSConfig *tls.Config
}
