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

	// StreamReadBufferSize is the maximum amount of data sent by the peer that a
	// stream will buffer for reading.
	// If zero, the default value of 1MiB is used.
	// If negative, the limit is zero.
	StreamReadBufferSize int64

	// StreamWriteBufferSize is the maximum amount of data a stream will buffer for
	// sending to the peer.
	// If zero, the default value of 1MiB is used.
	// If negative, the limit is zero.
	StreamWriteBufferSize int64
}

func configDefault(v, def int64) int64 {
	switch v {
	case -1:
		return 0
	case 0:
		return def
	}
	return v
}

func (c *Config) streamReadBufferSize() int64  { return configDefault(c.StreamReadBufferSize, 1<<20) }
func (c *Config) streamWriteBufferSize() int64 { return configDefault(c.StreamWriteBufferSize, 1<<20) }
