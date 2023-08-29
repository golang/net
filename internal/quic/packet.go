// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

// packetType is a QUIC packet type.
// https://www.rfc-editor.org/rfc/rfc9000.html#section-17
type packetType byte

const (
	packetTypeInvalid = packetType(iota)
	packetTypeInitial
	packetType0RTT
	packetTypeHandshake
	packetTypeRetry
	packetType1RTT
	packetTypeVersionNegotiation
)

// Bits set in the first byte of a packet.
const (
	headerFormLong  = 0x80 // https://www.rfc-editor.org/rfc/rfc9000.html#section-17.2-3.2.1
	headerFormShort = 0x00 // https://www.rfc-editor.org/rfc/rfc9000.html#section-17.3.1-4.2.1
	fixedBit        = 0x40 // https://www.rfc-editor.org/rfc/rfc9000.html#section-17.2-3.4.1
	reservedBits    = 0x0c // https://www.rfc-editor.org/rfc/rfc9000#section-17.2-8.2.1
)

// Long Packet Type bits.
// https://www.rfc-editor.org/rfc/rfc9000.html#section-17.2-3.6.1
const (
	longPacketTypeInitial   = 0 << 4
	longPacketType0RTT      = 1 << 4
	longPacketTypeHandshake = 2 << 4
	longPacketTypeRetry     = 3 << 4
)

// Frame types.
// https://www.rfc-editor.org/rfc/rfc9000.html#section-19
const (
	frameTypePadding                    = 0x00
	frameTypePing                       = 0x01
	frameTypeAck                        = 0x02
	frameTypeAckECN                     = 0x03
	frameTypeResetStream                = 0x04
	frameTypeStopSending                = 0x05
	frameTypeCrypto                     = 0x06
	frameTypeNewToken                   = 0x07
	frameTypeStreamBase                 = 0x08 // low three bits carry stream flags
	frameTypeMaxData                    = 0x10
	frameTypeMaxStreamData              = 0x11
	frameTypeMaxStreamsBidi             = 0x12
	frameTypeMaxStreamsUni              = 0x13
	frameTypeDataBlocked                = 0x14
	frameTypeStreamDataBlocked          = 0x15
	frameTypeStreamsBlockedBidi         = 0x16
	frameTypeStreamsBlockedUni          = 0x17
	frameTypeNewConnectionID            = 0x18
	frameTypeRetireConnectionID         = 0x19
	frameTypePathChallenge              = 0x1a
	frameTypePathResponse               = 0x1b
	frameTypeConnectionCloseTransport   = 0x1c
	frameTypeConnectionCloseApplication = 0x1d
	frameTypeHandshakeDone              = 0x1e
)

// The low three bits of STREAM frames.
// https://www.rfc-editor.org/rfc/rfc9000.html#section-19.8
const (
	streamOffBit = 0x04
	streamLenBit = 0x02
	streamFinBit = 0x01
)

// isLongHeader returns true if b is the first byte of a long header.
func isLongHeader(b byte) bool {
	return b&headerFormLong == headerFormLong
}

// getPacketType returns the type of a packet.
func getPacketType(b []byte) packetType {
	if len(b) == 0 {
		return packetTypeInvalid
	}
	if !isLongHeader(b[0]) {
		if b[0]&fixedBit != fixedBit {
			return packetTypeInvalid
		}
		return packetType1RTT
	}
	if len(b) < 5 {
		return packetTypeInvalid
	}
	if b[1] == 0 && b[2] == 0 && b[3] == 0 && b[4] == 0 {
		// Version Negotiation packets don't necessarily set the fixed bit.
		return packetTypeVersionNegotiation
	}
	if b[0]&fixedBit != fixedBit {
		return packetTypeInvalid
	}
	switch b[0] & 0x30 {
	case longPacketTypeInitial:
		return packetTypeInitial
	case longPacketType0RTT:
		return packetType0RTT
	case longPacketTypeHandshake:
		return packetTypeHandshake
	case longPacketTypeRetry:
		return packetTypeRetry
	}
	return packetTypeInvalid
}

// dstConnIDForDatagram returns the destination connection ID field of the
// first QUIC packet in a datagram.
func dstConnIDForDatagram(pkt []byte) (id []byte, ok bool) {
	if len(pkt) < 1 {
		return nil, false
	}
	var n int
	var b []byte
	if isLongHeader(pkt[0]) {
		if len(pkt) < 6 {
			return nil, false
		}
		n = int(pkt[5])
		b = pkt[6:]
	} else {
		n = connIDLen
		b = pkt[1:]
	}
	if len(b) < n {
		return nil, false
	}
	return b[:n], true
}

// A longPacket is a long header packet.
type longPacket struct {
	ptype        packetType
	reservedBits uint8
	version      uint32
	num          packetNumber
	dstConnID    []byte
	srcConnID    []byte
	payload      []byte

	// The extra data depends on the packet type:
	//   Initial: Token.
	//   Retry: Retry token and integrity tag.
	extra []byte
}

// A shortPacket is a short header (1-RTT) packet.
type shortPacket struct {
	reservedBits uint8
	num          packetNumber
	payload      []byte
}
