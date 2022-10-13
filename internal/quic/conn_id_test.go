// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"fmt"
	"reflect"
	"testing"
)

func TestConnIDClientHandshake(t *testing.T) {
	// On initialization, the client chooses local and remote IDs.
	//
	// The order in which we allocate the two isn't actually important,
	// but test is a lot simpler if we assume.
	var s connIDState
	s.initClient(newConnIDSequence())
	if got, want := string(s.srcConnID()), "local-1"; got != want {
		t.Errorf("after initClient: srcConnID = %q, want %q", got, want)
	}
	if got, want := string(s.dstConnID()), "local-2"; got != want {
		t.Errorf("after initClient: dstConnID = %q, want %q", got, want)
	}

	// The server's first Initial packet provides the client with a
	// non-transient remote connection ID.
	s.handlePacket(clientSide, packetTypeInitial, []byte("remote-1"))
	if got, want := string(s.dstConnID()), "remote-1"; got != want {
		t.Errorf("after receiving Initial: dstConnID = %q, want %q", got, want)
	}

	wantLocal := []connID{{
		cid: []byte("local-1"),
		seq: 0,
	}}
	if !reflect.DeepEqual(s.local, wantLocal) {
		t.Errorf("local ids: %v, want %v", s.local, wantLocal)
	}
	wantRemote := []connID{{
		cid: []byte("remote-1"),
		seq: 0,
	}}
	if !reflect.DeepEqual(s.remote, wantRemote) {
		t.Errorf("remote ids: %v, want %v", s.remote, wantRemote)
	}
}

func TestConnIDServerHandshake(t *testing.T) {
	// On initialization, the server is provided with the client-chosen
	// transient connection ID, and allocates an ID of its own.
	// The Initial packet sets the remote connection ID.
	var s connIDState
	s.initServer(newConnIDSequence(), []byte("transient"))
	s.handlePacket(serverSide, packetTypeInitial, []byte("remote-1"))
	if got, want := string(s.srcConnID()), "local-1"; got != want {
		t.Errorf("after initClient: srcConnID = %q, want %q", got, want)
	}
	if got, want := string(s.dstConnID()), "remote-1"; got != want {
		t.Errorf("after initClient: dstConnID = %q, want %q", got, want)
	}

	wantLocal := []connID{{
		cid: []byte("transient"),
		seq: -1,
	}, {
		cid: []byte("local-1"),
		seq: 0,
	}}
	if !reflect.DeepEqual(s.local, wantLocal) {
		t.Errorf("local ids: %v, want %v", s.local, wantLocal)
	}
	wantRemote := []connID{{
		cid: []byte("remote-1"),
		seq: 0,
	}}
	if !reflect.DeepEqual(s.remote, wantRemote) {
		t.Errorf("remote ids: %v, want %v", s.remote, wantRemote)
	}

	// The client's first Handshake packet permits the server to discard the
	// transient connection ID.
	s.handlePacket(serverSide, packetTypeHandshake, []byte("remote-1"))
	wantLocal = []connID{{
		cid: []byte("local-1"),
		seq: 0,
	}}
	if !reflect.DeepEqual(s.local, wantLocal) {
		t.Errorf("after handshake local ids: %v, want %v", s.local, wantLocal)
	}
}

func newConnIDSequence() newConnIDFunc {
	var n uint64
	return func() ([]byte, error) {
		n++
		return []byte(fmt.Sprintf("local-%v", n)), nil
	}
}

func TestNewRandomConnID(t *testing.T) {
	cid, err := newRandomConnID()
	if len(cid) != connIDLen || err != nil {
		t.Fatalf("newConnID() = %x, %v; want %v bytes", cid, connIDLen, err)
	}
}
