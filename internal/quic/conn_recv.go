// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"time"
)

func (c *Conn) handleDatagram(now time.Time, dgram *datagram) {
	buf := dgram.b
	c.loss.datagramReceived(now, len(buf))
	for len(buf) > 0 {
		var n int
		ptype := getPacketType(buf)
		switch ptype {
		case packetTypeInitial:
			if c.side == serverSide && len(dgram.b) < minimumClientInitialDatagramSize {
				// Discard client-sent Initial packets in too-short datagrams.
				// https://www.rfc-editor.org/rfc/rfc9000#section-14.1-4
				return
			}
			n = c.handleLongHeader(now, ptype, initialSpace, buf)
		case packetTypeHandshake:
			n = c.handleLongHeader(now, ptype, handshakeSpace, buf)
		case packetType1RTT:
			n = c.handle1RTT(now, buf)
		default:
			return
		}
		if n <= 0 {
			// Invalid data at the end of a datagram is ignored.
			break
		}
		c.idleTimeout = now.Add(c.maxIdleTimeout)
		buf = buf[n:]
	}
}

func (c *Conn) handleLongHeader(now time.Time, ptype packetType, space numberSpace, buf []byte) int {
	if !c.rkeys[space].isSet() {
		return skipLongHeaderPacket(buf)
	}

	pnumMax := c.acks[space].largestSeen()
	p, n := parseLongHeaderPacket(buf, c.rkeys[space], pnumMax)
	if n < 0 {
		return -1
	}
	if p.reservedBits != 0 {
		// https://www.rfc-editor.org/rfc/rfc9000#section-17.2-8.2.1
		c.abort(now, localTransportError(errProtocolViolation))
		return -1
	}

	if !c.acks[space].shouldProcess(p.num) {
		return n
	}

	if logPackets {
		logInboundLongPacket(c, p)
	}
	c.connIDState.handlePacket(c.side, p.ptype, p.srcConnID)
	ackEliciting := c.handleFrames(now, ptype, space, p.payload)
	c.acks[space].receive(now, space, p.num, ackEliciting)
	if p.ptype == packetTypeHandshake && c.side == serverSide {
		c.loss.validateClientAddress()

		// "[...] a server MUST discard Initial keys when it first successfully
		// processes a Handshake packet [...]"
		// https://www.rfc-editor.org/rfc/rfc9001#section-4.9.1-2
		c.discardKeys(now, initialSpace)
	}
	return n
}

func (c *Conn) handle1RTT(now time.Time, buf []byte) int {
	if !c.rkeys[appDataSpace].isSet() {
		// 1-RTT packets extend to the end of the datagram,
		// so skip the remainder of the datagram if we can't parse this.
		return len(buf)
	}

	pnumMax := c.acks[appDataSpace].largestSeen()
	p, n := parse1RTTPacket(buf, c.rkeys[appDataSpace], connIDLen, pnumMax)
	if n < 0 {
		return -1
	}
	if p.reservedBits != 0 {
		// https://www.rfc-editor.org/rfc/rfc9000#section-17.3.1-4.8.1
		c.abort(now, localTransportError(errProtocolViolation))
		return -1
	}

	if !c.acks[appDataSpace].shouldProcess(p.num) {
		return len(buf)
	}

	if logPackets {
		logInboundShortPacket(c, p)
	}
	ackEliciting := c.handleFrames(now, packetType1RTT, appDataSpace, p.payload)
	c.acks[appDataSpace].receive(now, appDataSpace, p.num, ackEliciting)
	return len(buf)
}

func (c *Conn) handleFrames(now time.Time, ptype packetType, space numberSpace, payload []byte) (ackEliciting bool) {
	if len(payload) == 0 {
		// "An endpoint MUST treat receipt of a packet containing no frames
		// as a connection error of type PROTOCOL_VIOLATION."
		// https://www.rfc-editor.org/rfc/rfc9000#section-12.4-3
		c.abort(now, localTransportError(errProtocolViolation))
		return false
	}
	// frameOK verifies that ptype is one of the packets in mask.
	frameOK := func(c *Conn, ptype, mask packetType) (ok bool) {
		if ptype&mask == 0 {
			// "An endpoint MUST treat receipt of a frame in a packet type
			// that is not permitted as a connection error of type
			// PROTOCOL_VIOLATION."
			// https://www.rfc-editor.org/rfc/rfc9000#section-12.4-3
			c.abort(now, localTransportError(errProtocolViolation))
			return false
		}
		return true
	}
	// Packet masks from RFC 9000 Table 3.
	// https://www.rfc-editor.org/rfc/rfc9000#table-3
	const (
		IH_1 = packetTypeInitial | packetTypeHandshake | packetType1RTT
		__01 = packetType0RTT | packetType1RTT
		___1 = packetType1RTT
	)
	for len(payload) > 0 {
		switch payload[0] {
		case frameTypePadding, frameTypeAck, frameTypeAckECN,
			frameTypeConnectionCloseTransport, frameTypeConnectionCloseApplication:
		default:
			ackEliciting = true
		}
		n := -1
		switch payload[0] {
		case frameTypePadding:
			// PADDING is OK in all spaces.
			n = 1
		case frameTypePing:
			// PING is OK in all spaces.
			//
			// A PING frame causes us to respond with an ACK by virtue of being
			// an ack-eliciting frame, but requires no other action.
			n = 1
		case frameTypeAck, frameTypeAckECN:
			if !frameOK(c, ptype, IH_1) {
				return
			}
			n = c.handleAckFrame(now, space, payload)
		case frameTypeResetStream:
			if !frameOK(c, ptype, __01) {
				return
			}
			_, _, _, n = consumeResetStreamFrame(payload)
		case frameTypeStopSending:
			if !frameOK(c, ptype, __01) {
				return
			}
			_, _, n = consumeStopSendingFrame(payload)
		case frameTypeCrypto:
			if !frameOK(c, ptype, IH_1) {
				return
			}
			n = c.handleCryptoFrame(now, space, payload)
		case frameTypeNewToken:
			if !frameOK(c, ptype, ___1) {
				return
			}
			_, n = consumeNewTokenFrame(payload)
		case 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f: // STREAM
			if !frameOK(c, ptype, __01) {
				return
			}
			n = c.handleStreamFrame(now, space, payload)
		case frameTypeMaxData:
			if !frameOK(c, ptype, __01) {
				return
			}
			_, n = consumeMaxDataFrame(payload)
		case frameTypeMaxStreamData:
			if !frameOK(c, ptype, __01) {
				return
			}
			_, _, n = consumeMaxStreamDataFrame(payload)
		case frameTypeMaxStreamsBidi, frameTypeMaxStreamsUni:
			if !frameOK(c, ptype, __01) {
				return
			}
			_, _, n = consumeMaxStreamsFrame(payload)
		case frameTypeStreamsBlockedBidi, frameTypeStreamsBlockedUni:
			if !frameOK(c, ptype, __01) {
				return
			}
			_, _, n = consumeStreamsBlockedFrame(payload)
		case frameTypeStreamDataBlocked:
			if !frameOK(c, ptype, __01) {
				return
			}
			_, _, n = consumeStreamDataBlockedFrame(payload)
		case frameTypeNewConnectionID:
			if !frameOK(c, ptype, __01) {
				return
			}
			n = c.handleNewConnectionIDFrame(now, space, payload)
		case frameTypeRetireConnectionID:
			if !frameOK(c, ptype, __01) {
				return
			}
			n = c.handleRetireConnectionIDFrame(now, space, payload)
		case frameTypeConnectionCloseTransport:
			// CONNECTION_CLOSE is OK in all spaces.
			_, _, _, n = consumeConnectionCloseTransportFrame(payload)
			// TODO: https://www.rfc-editor.org/rfc/rfc9000.html#section-10.2.2
			c.abort(now, localTransportError(errNo))
		case frameTypeConnectionCloseApplication:
			// CONNECTION_CLOSE is OK in all spaces.
			_, _, n = consumeConnectionCloseApplicationFrame(payload)
			// TODO: https://www.rfc-editor.org/rfc/rfc9000.html#section-10.2.2
			c.abort(now, localTransportError(errNo))
		case frameTypeHandshakeDone:
			if !frameOK(c, ptype, ___1) {
				return
			}
			n = c.handleHandshakeDoneFrame(now, space, payload)
		}
		if n < 0 {
			c.abort(now, localTransportError(errFrameEncoding))
			return false
		}
		payload = payload[n:]
	}
	return ackEliciting
}

func (c *Conn) handleAckFrame(now time.Time, space numberSpace, payload []byte) int {
	c.loss.receiveAckStart()
	_, ackDelay, n := consumeAckFrame(payload, func(rangeIndex int, start, end packetNumber) {
		if end > c.loss.nextNumber(space) {
			// Acknowledgement of a packet we never sent.
			c.abort(now, localTransportError(errProtocolViolation))
			return
		}
		c.loss.receiveAckRange(now, space, rangeIndex, start, end, c.handleAckOrLoss)
	})
	// Prior to receiving the peer's transport parameters, we cannot
	// interpret the ACK Delay field because we don't know the ack_delay_exponent
	// to apply.
	//
	// For servers, we should always know the ack_delay_exponent because the
	// client's transport parameters are carried in its Initial packets and we
	// won't send an ack-eliciting Initial packet until after receiving the last
	// client Initial packet.
	//
	// For clients, we won't receive the server's transport parameters until handling
	// its Handshake flight, which will probably happen after reading its ACK for our
	// Initial packet(s). However, the peer's acknowledgement delay cannot reduce our
	// adjusted RTT sample below min_rtt, and min_rtt is generally going to be set
	// by the packet containing the ACK for our Initial flight. Therefore, the
	// ACK Delay for an ACK in the Initial space is likely to be ignored anyway.
	//
	// Long story short, setting the delay to 0 prior to reading transport parameters
	// is usually going to have no effect, will have only a minor effect in the rare
	// cases when it happens, and there aren't any good alternatives anyway since we
	// can't interpret the ACK Delay field without knowing the exponent.
	var delay time.Duration
	if c.peerAckDelayExponent >= 0 {
		delay = ackDelay.Duration(uint8(c.peerAckDelayExponent))
	}
	c.loss.receiveAckEnd(now, space, delay, c.handleAckOrLoss)
	return n
}

func (c *Conn) handleCryptoFrame(now time.Time, space numberSpace, payload []byte) int {
	off, data, n := consumeCryptoFrame(payload)
	err := c.handleCrypto(now, space, off, data)
	if err != nil {
		c.abort(now, err)
		return -1
	}
	return n
}

func (c *Conn) handleStreamFrame(now time.Time, space numberSpace, payload []byte) int {
	id, off, fin, b, n := consumeStreamFrame(payload)
	if n < 0 {
		return -1
	}
	if s := c.streamForFrame(now, id, recvStream); s != nil {
		if err := s.handleData(off, b, fin); err != nil {
			c.abort(now, err)
		}
	}
	return n
}

func (c *Conn) handleNewConnectionIDFrame(now time.Time, space numberSpace, payload []byte) int {
	seq, retire, connID, resetToken, n := consumeNewConnectionIDFrame(payload)
	if n < 0 {
		return -1
	}
	if err := c.connIDState.handleNewConnID(seq, retire, connID, resetToken); err != nil {
		c.abort(now, err)
	}
	return n
}

func (c *Conn) handleRetireConnectionIDFrame(now time.Time, space numberSpace, payload []byte) int {
	seq, n := consumeRetireConnectionIDFrame(payload)
	if n < 0 {
		return -1
	}
	if err := c.connIDState.handleRetireConnID(seq, c.newConnIDFunc()); err != nil {
		c.abort(now, err)
	}
	return n
}

func (c *Conn) handleHandshakeDoneFrame(now time.Time, space numberSpace, payload []byte) int {
	if c.side == serverSide {
		// Clients should never send HANDSHAKE_DONE.
		// https://www.rfc-editor.org/rfc/rfc9000#section-19.20-4
		c.abort(now, localTransportError(errProtocolViolation))
		return -1
	}
	c.confirmHandshake(now)
	return 1
}
