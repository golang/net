// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"bytes"
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
	//
	// These are []connID rather than []*connID to minimize allocations.
	local  []connID
	remote []connID

	nextLocalSeq          int64
	retireRemotePriorTo   int64 // largest Retire Prior To value sent by the peer
	peerActiveConnIDLimit int64 // peer's active_connection_id_limit transport parameter

	needSend bool
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

	// retired is set when the connection ID is retired.
	retired bool

	// send is set when the connection ID's state needs to be sent to the peer.
	//
	// For local IDs, this indicates a new ID that should be sent
	// in a NEW_CONNECTION_ID frame.
	//
	// For remote IDs, this indicates a retired ID that should be sent
	// in a RETIRE_CONNECTION_ID frame.
	send sentVal
}

func (s *connIDState) initClient(newID newConnIDFunc) error {
	// Client chooses its initial connection ID, and sends it
	// in the Source Connection ID field of the first Initial packet.
	locid, err := newID(0)
	if err != nil {
		return err
	}
	s.local = append(s.local, connID{
		seq: 0,
		cid: locid,
	})
	s.nextLocalSeq = 1

	// Client chooses an initial, transient connection ID for the server,
	// and sends it in the Destination Connection ID field of the first Initial packet.
	remid, err := newID(-1)
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
	locid, err := newID(0)
	if err != nil {
		return err
	}
	s.local = append(s.local, connID{
		seq: 0,
		cid: locid,
	})
	s.nextLocalSeq = 1
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
func (s *connIDState) dstConnID() (cid []byte, ok bool) {
	for i := range s.remote {
		if !s.remote[i].retired {
			return s.remote[i].cid, true
		}
	}
	return nil, false
}

// setPeerActiveConnIDLimit sets the active_connection_id_limit
// transport parameter received from the peer.
func (s *connIDState) setPeerActiveConnIDLimit(lim int64, newID newConnIDFunc) error {
	s.peerActiveConnIDLimit = lim
	return s.issueLocalIDs(newID)
}

func (s *connIDState) issueLocalIDs(newID newConnIDFunc) error {
	toIssue := min(int(s.peerActiveConnIDLimit), maxPeerActiveConnIDLimit)
	for i := range s.local {
		if s.local[i].seq != -1 && !s.local[i].retired {
			toIssue--
		}
	}
	for toIssue > 0 {
		cid, err := newID(s.nextLocalSeq)
		if err != nil {
			return err
		}
		s.local = append(s.local, connID{
			seq: s.nextLocalSeq,
			cid: cid,
		})
		s.local[len(s.local)-1].send.setUnsent()
		s.nextLocalSeq++
		s.needSend = true
		toIssue--
	}
	return nil
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

func (s *connIDState) handleNewConnID(seq, retire int64, cid []byte, resetToken [16]byte) error {
	if len(s.remote[0].cid) == 0 {
		// "An endpoint that is sending packets with a zero-length
		// Destination Connection ID MUST treat receipt of a NEW_CONNECTION_ID
		// frame as a connection error of type PROTOCOL_VIOLATION."
		// https://www.rfc-editor.org/rfc/rfc9000.html#section-19.15-6
		return localTransportError(errProtocolViolation)
	}

	if retire > s.retireRemotePriorTo {
		s.retireRemotePriorTo = retire
	}

	have := false // do we already have this connection ID?
	active := 0
	for i := range s.remote {
		rcid := &s.remote[i]
		if !rcid.retired && rcid.seq < s.retireRemotePriorTo {
			s.retireRemote(rcid)
		}
		if !rcid.retired {
			active++
		}
		if rcid.seq == seq {
			if !bytes.Equal(rcid.cid, cid) {
				return localTransportError(errProtocolViolation)
			}
			have = true // yes, we've seen this sequence number
		}
	}

	if !have {
		// This is a new connection ID that we have not seen before.
		//
		// We could take steps to keep the list of remote connection IDs
		// sorted by sequence number, but there's no particular need
		// so we don't bother.
		s.remote = append(s.remote, connID{
			seq: seq,
			cid: cloneBytes(cid),
		})
		if seq < s.retireRemotePriorTo {
			// This ID was already retired by a previous NEW_CONNECTION_ID frame.
			s.retireRemote(&s.remote[len(s.remote)-1])
		} else {
			active++
		}
	}

	if active > activeConnIDLimit {
		// Retired connection IDs (including newly-retired ones) do not count
		// against the limit.
		// https://www.rfc-editor.org/rfc/rfc9000.html#section-5.1.1-5
		return localTransportError(errConnectionIDLimit)
	}

	// "An endpoint SHOULD limit the number of connection IDs it has retired locally
	// for which RETIRE_CONNECTION_ID frames have not yet been acknowledged."
	// https://www.rfc-editor.org/rfc/rfc9000#section-5.1.2-6
	//
	// Set a limit of four times the active_connection_id_limit for
	// the total number of remote connection IDs we keep state for locally.
	if len(s.remote) > 4*activeConnIDLimit {
		return localTransportError(errConnectionIDLimit)
	}

	return nil
}

// retireRemote marks a remote connection ID as retired.
func (s *connIDState) retireRemote(rcid *connID) {
	rcid.retired = true
	rcid.send.setUnsent()
	s.needSend = true
}

func (s *connIDState) handleRetireConnID(seq int64, newID newConnIDFunc) error {
	if seq >= s.nextLocalSeq {
		return localTransportError(errProtocolViolation)
	}
	for i := range s.local {
		if s.local[i].seq == seq {
			s.local = append(s.local[:i], s.local[i+1:]...)
			break
		}
	}
	s.issueLocalIDs(newID)
	return nil
}

func (s *connIDState) ackOrLossNewConnectionID(pnum packetNumber, seq int64, fate packetFate) {
	for i := range s.local {
		if s.local[i].seq != seq {
			continue
		}
		s.local[i].send.ackOrLoss(pnum, fate)
		if fate != packetAcked {
			s.needSend = true
		}
		return
	}
}

func (s *connIDState) ackOrLossRetireConnectionID(pnum packetNumber, seq int64, fate packetFate) {
	for i := 0; i < len(s.remote); i++ {
		if s.remote[i].seq != seq {
			continue
		}
		if fate == packetAcked {
			// We have retired this connection ID, and the peer has acked.
			// Discard its state completely.
			s.remote = append(s.remote[:i], s.remote[i+1:]...)
		} else {
			// RETIRE_CONNECTION_ID frame was lost, mark for retransmission.
			s.needSend = true
			s.remote[i].send.ackOrLoss(pnum, fate)
		}
		return
	}
}

// appendFrames appends NEW_CONNECTION_ID and RETIRE_CONNECTION_ID frames
// to the current packet.
//
// It returns true if no more frames need appending,
// false if not everything fit in the current packet.
func (s *connIDState) appendFrames(w *packetWriter, pnum packetNumber, pto bool) bool {
	if !s.needSend && !pto {
		// Fast path: We don't need to send anything.
		return true
	}
	retireBefore := int64(0)
	if s.local[0].seq != -1 {
		retireBefore = s.local[0].seq
	}
	for i := range s.local {
		if !s.local[i].send.shouldSendPTO(pto) {
			continue
		}
		if !w.appendNewConnectionIDFrame(
			s.local[i].seq,
			retireBefore,
			s.local[i].cid,
			[16]byte{}, // TODO: stateless reset token
		) {
			return false
		}
		s.local[i].send.setSent(pnum)
	}
	for i := range s.remote {
		if !s.remote[i].send.shouldSendPTO(pto) {
			continue
		}
		if !w.appendRetireConnectionIDFrame(s.remote[i].seq) {
			return false
		}
		s.remote[i].send.setSent(pnum)
	}
	s.needSend = false
	return true
}

func cloneBytes(b []byte) []byte {
	n := make([]byte, len(b))
	copy(n, b)
	return n
}

type newConnIDFunc func(seq int64) ([]byte, error)

func newRandomConnID(_ int64) ([]byte, error) {
	// It is not necessary for connection IDs to be cryptographically secure,
	// but it doesn't hurt.
	id := make([]byte, connIDLen)
	if _, err := rand.Read(id); err != nil {
		// TODO: Surface this error as a metric or log event or something.
		// rand.Read really shouldn't ever fail, but if it does, we should
		// have a way to inform the user.
		return nil, err
	}
	return id, nil
}
