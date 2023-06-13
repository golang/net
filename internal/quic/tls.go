// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

// tlsState encapsulates interactions with TLS.
type tlsState struct {
	// Encryption keys indexed by number space.
	rkeys [numberSpaceCount]keys
	wkeys [numberSpaceCount]keys
}

func (s *tlsState) init(side connSide, initialConnID []byte) {
	clientKeys, serverKeys := initialKeys(initialConnID)
	if side == clientSide {
		s.wkeys[initialSpace], s.rkeys[initialSpace] = clientKeys, serverKeys
	} else {
		s.wkeys[initialSpace], s.rkeys[initialSpace] = serverKeys, clientKeys
	}
}
