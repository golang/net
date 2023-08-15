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

	// MaxBidiRemoteStreams limits the number of simultaneous bidirectional streams
	// a peer may open.
	// If zero, the default value of 100 is used.
	// If negative, the limit is zero.
	MaxBidiRemoteStreams int64

	// MaxUniRemoteStreams limits the number of simultaneous unidirectional streams
	// a peer may open.
	// If zero, the default value of 100 is used.
	// If negative, the limit is zero.
	MaxUniRemoteStreams int64

	// MaxStreamReadBufferSize is the maximum amount of data sent by the peer that a
	// stream will buffer for reading.
	// If zero, the default value of 1MiB is used.
	// If negative, the limit is zero.
	MaxStreamReadBufferSize int64

	// MaxStreamWriteBufferSize is the maximum amount of data a stream will buffer for
	// sending to the peer.
	// If zero, the default value of 1MiB is used.
	// If negative, the limit is zero.
	MaxStreamWriteBufferSize int64

	// MaxConnReadBufferSize is the maximum amount of data sent by the peer that a
	// connection will buffer for reading, across all streams.
	// If zero, the default value of 1MiB is used.
	// If negative, the limit is zero.
	MaxConnReadBufferSize int64
}

func configDefault(v, def, limit int64) int64 {
	switch {
	case v == 0:
		return def
	case v < 0:
		return 0
	default:
		return min(v, limit)
	}
}

func (c *Config) maxBidiRemoteStreams() int64 {
	return configDefault(c.MaxBidiRemoteStreams, 100, maxStreamsLimit)
}

func (c *Config) maxUniRemoteStreams() int64 {
	return configDefault(c.MaxUniRemoteStreams, 100, maxStreamsLimit)
}

func (c *Config) maxStreamReadBufferSize() int64 {
	return configDefault(c.MaxStreamReadBufferSize, 1<<20, maxVarint)
}

func (c *Config) maxStreamWriteBufferSize() int64 {
	return configDefault(c.MaxStreamWriteBufferSize, 1<<20, maxVarint)
}

func (c *Config) maxConnReadBufferSize() int64 {
	return configDefault(c.MaxConnReadBufferSize, 1<<20, maxVarint)
}
