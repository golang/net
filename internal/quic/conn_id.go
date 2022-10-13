// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"crypto/rand"
)

// connIDState is a conn's connection IDs.
type connIDState struct {
	// The destination connection IDs of packets we receive are local.
	// The destination connection IDs of packets we send are remote.
	//
	// Local IDs are usually issued by us, and remote IDs by the peer.
	// The exception is the transient destination connection ID sent in
	// a client's Initial packets, which is chosen by the client.
	local  []connID
	remote []connID
}

// A connID is a connection ID and associated metadata.
type connID struct {
	// cid is the connection ID itself.
	cid []byte

	// seq is the connection ID's sequence number:
	// https://www.rfc-editor.org/rfc/rfc9000.html#section-5.1.1-1
	//
	// For the transient destination ID in a client's Initial packet, this is -1.
	seq int64
}

func (s *connIDState) initClient(newID newConnIDFunc) error {
	// Client chooses its initial connection ID, and sends it
	// in the Source Connection ID field of the first Initial packet.
	locid, err := newID()
	if err != nil {
		return err
	}
	s.local = append(s.local, connID{
		seq: 0,
		cid: locid,
	})

	// Client chooses an initial, transient connection ID for the server,
	// and sends it in the Destination Connection ID field of the first Initial packet.
	remid, err := newID()
	if err != nil {
		return err
	}
	s.remote = append(s.remote, connID{
		seq: -1,
		cid: remid,
	})
	return nil
}

func (s *connIDState) initServer(newID newConnIDFunc, dstConnID []byte) error {
	// Client-chosen, transient connection ID received in the first Initial packet.
	// The server will not use this as the Source Connection ID of packets it sends,
	// but remembers it because it may receive packets sent to this destination.
	s.local = append(s.local, connID{
		seq: -1,
		cid: cloneBytes(dstConnID),
	})

	// Server chooses a connection ID, and sends it in the Source Connection ID of
	// the response to the clent.
	locid, err := newID()
	if err != nil {
		return err
	}
	s.local = append(s.local, connID{
		seq: 0,
		cid: locid,
	})
	return nil
}

// srcConnID is the Source Connection ID to use in a sent packet.
func (s *connIDState) srcConnID() []byte {
	if s.local[0].seq == -1 && len(s.local) > 1 {
		// Don't use the transient connection ID if another is available.
		return s.local[1].cid
	}
	return s.local[0].cid
}

// dstConnID is the Destination Connection ID to use in a sent packet.
func (s *connIDState) dstConnID() []byte {
	return s.remote[0].cid
}

// handlePacket updates the connection ID state during the handshake
// (Initial and Handshake packets).
func (s *connIDState) handlePacket(side connSide, ptype packetType, srcConnID []byte) {
	switch {
	case ptype == packetTypeInitial && side == clientSide:
		if len(s.remote) == 1 && s.remote[0].seq == -1 {
			// We're a client connection processing the first Initial packet
			// from the server. Replace the transient remote connection ID
			// with the Source Connection ID from the packet.
			s.remote[0] = connID{
				seq: 0,
				cid: cloneBytes(srcConnID),
			}
		}
	case ptype == packetTypeInitial && side == serverSide:
		if len(s.remote) == 0 {
			// We're a server connection processing the first Initial packet
			// from the client. Set the client's connection ID.
			s.remote = append(s.remote, connID{
				seq: 0,
				cid: cloneBytes(srcConnID),
			})
		}
	case ptype == packetTypeHandshake && side == serverSide:
		if len(s.local) > 0 && s.local[0].seq == -1 {
			// We're a server connection processing the first Handshake packet from
			// the client. Discard the transient, client-chosen connection ID used
			// for Initial packets; the client will never send it again.
			s.local = append(s.local[:0], s.local[1:]...)
		}
	}
}

func cloneBytes(b []byte) []byte {
	n := make([]byte, len(b))
	copy(n, b)
	return n
}

type newConnIDFunc func() ([]byte, error)

func newRandomConnID() ([]byte, error) {
	// It is not necessary for connection IDs to be cryptographically secure,
	// but it doesn't hurt.
	id := make([]byte, connIDLen)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	return id, nil
}
