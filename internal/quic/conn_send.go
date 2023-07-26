// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"crypto/tls"
	"errors"
	"time"
)

// maybeSend sends datagrams, if possible.
//
// If sending is blocked by pacing, it returns the next time
// a datagram may be sent.
func (c *Conn) maybeSend(now time.Time) (next time.Time) {
	// Assumption: The congestion window is not underutilized.
	// If congestion control, pacing, and anti-amplification all permit sending,
	// but we have no packet to send, then we will declare the window underutilized.
	c.loss.cc.setUnderutilized(false)

	// Send one datagram on each iteration of this loop,
	// until we hit a limit or run out of data to send.
	//
	// For each number space where we have write keys,
	// attempt to construct a packet in that space.
	// If the packet contains no frames (we have no data in need of sending),
	// abandon the packet.
	//
	// Speculatively constructing packets means we don't need
	// separate code paths for "do we have data to send?" and
	// "send the data" that need to be kept in sync.
	for {
		limit, next := c.loss.sendLimit(now)
		if limit == ccBlocked {
			// If anti-amplification blocks sending, then no packet can be sent.
			return next
		}
		// We may still send ACKs, even if congestion control or pacing limit sending.

		// Prepare to write a datagram of at most maxSendSize bytes.
		c.w.reset(c.loss.maxSendSize())

		dstConnID, ok := c.connIDState.dstConnID()
		if !ok {
			// It is currently not possible for us to end up without a connection ID,
			// but handle the case anyway.
			return time.Time{}
		}

		// Initial packet.
		pad := false
		var sentInitial *sentPacket
		if k := c.wkeys[initialSpace]; k.isSet() {
			pnumMaxAcked := c.acks[initialSpace].largestSeen()
			pnum := c.loss.nextNumber(initialSpace)
			p := longPacket{
				ptype:     packetTypeInitial,
				version:   1,
				num:       pnum,
				dstConnID: dstConnID,
				srcConnID: c.connIDState.srcConnID(),
			}
			c.w.startProtectedLongHeaderPacket(pnumMaxAcked, p)
			c.appendFrames(now, initialSpace, pnum, limit)
			if logPackets {
				logSentPacket(c, packetTypeInitial, pnum, p.srcConnID, p.dstConnID, c.w.payload())
			}
			sentInitial = c.w.finishProtectedLongHeaderPacket(pnumMaxAcked, k, p)
			if sentInitial != nil {
				// Client initial packets need to be sent in a datagram padded to
				// at least 1200 bytes. We can't add the padding yet, however,
				// since we may want to coalesce additional packets with this one.
				if c.side == clientSide {
					pad = true
				}
			}
		}

		// Handshake packet.
		if k := c.wkeys[handshakeSpace]; k.isSet() {
			pnumMaxAcked := c.acks[handshakeSpace].largestSeen()
			pnum := c.loss.nextNumber(handshakeSpace)
			p := longPacket{
				ptype:     packetTypeHandshake,
				version:   1,
				num:       pnum,
				dstConnID: dstConnID,
				srcConnID: c.connIDState.srcConnID(),
			}
			c.w.startProtectedLongHeaderPacket(pnumMaxAcked, p)
			c.appendFrames(now, handshakeSpace, pnum, limit)
			if logPackets {
				logSentPacket(c, packetTypeHandshake, pnum, p.srcConnID, p.dstConnID, c.w.payload())
			}
			if sent := c.w.finishProtectedLongHeaderPacket(pnumMaxAcked, k, p); sent != nil {
				c.loss.packetSent(now, handshakeSpace, sent)
				if c.side == clientSide {
					// "[...] a client MUST discard Initial keys when it first
					// sends a Handshake packet [...]"
					// https://www.rfc-editor.org/rfc/rfc9001.html#section-4.9.1-2
					c.discardKeys(now, initialSpace)
				}
			}
		}

		// 1-RTT packet.
		if k := c.wkeys[appDataSpace]; k.isSet() {
			pnumMaxAcked := c.acks[appDataSpace].largestSeen()
			pnum := c.loss.nextNumber(appDataSpace)
			c.w.start1RTTPacket(pnum, pnumMaxAcked, dstConnID)
			c.appendFrames(now, appDataSpace, pnum, limit)
			if pad && len(c.w.payload()) > 0 {
				// 1-RTT packets have no length field and extend to the end
				// of the datagram, so if we're sending a datagram that needs
				// padding we need to add it inside the 1-RTT packet.
				c.w.appendPaddingTo(minimumClientInitialDatagramSize)
				pad = false
			}
			if logPackets {
				logSentPacket(c, packetType1RTT, pnum, nil, dstConnID, c.w.payload())
			}
			if sent := c.w.finish1RTTPacket(pnum, pnumMaxAcked, dstConnID, k); sent != nil {
				c.loss.packetSent(now, appDataSpace, sent)
			}
		}

		buf := c.w.datagram()
		if len(buf) == 0 {
			if limit == ccOK {
				// We have nothing to send, and congestion control does not
				// block sending. The congestion window is underutilized.
				c.loss.cc.setUnderutilized(true)
			}
			return next
		}

		if sentInitial != nil {
			if pad {
				// Pad out the datagram with zeros, coalescing the Initial
				// packet with invalid packets that will be ignored by the peer.
				// https://www.rfc-editor.org/rfc/rfc9000.html#section-14.1-1
				for len(buf) < minimumClientInitialDatagramSize {
					buf = append(buf, 0)
					// Technically this padding isn't in any packet, but
					// account it to the Initial packet in this datagram
					// for purposes of flow control and loss recovery.
					sentInitial.size++
					sentInitial.inFlight = true
				}
			}
			if k := c.wkeys[initialSpace]; k.isSet() {
				c.loss.packetSent(now, initialSpace, sentInitial)
			}
		}

		c.listener.sendDatagram(buf, c.peerAddr)
	}
}

func (c *Conn) appendFrames(now time.Time, space numberSpace, pnum packetNumber, limit ccLimit) {
	if c.errForPeer != nil {
		// This is the bare minimum required to send a CONNECTION_CLOSE frame
		// when closing a connection immediately, for example in response to a
		// protocol error.
		//
		// This does not handle the closing and draining states
		// (https://www.rfc-editor.org/rfc/rfc9000.html#section-10.2),
		// but it's enough to let us write tests that result in a CONNECTION_CLOSE,
		// and have those tests still pass when we finish implementing
		// connection shutdown.
		//
		// TODO: Finish implementing connection shutdown.
		if !c.connCloseSent[space] {
			c.exited = true
			c.appendConnectionCloseFrame(c.errForPeer)
			c.connCloseSent[space] = true
		}
		return
	}

	shouldSendAck := c.acks[space].shouldSendAck(now)
	if limit != ccOK {
		// ACKs are not limited by congestion control.
		if shouldSendAck && c.appendAckFrame(now, space) {
			c.acks[space].sentAck()
		}
		return
	}
	// We want to send an ACK frame if the ack controller wants to send a frame now,
	// OR if we are sending a packet anyway and have ack-eliciting packets which we
	// have not yet acked.
	//
	// We speculatively add ACK frames here, to put them at the front of the packet
	// to avoid truncation.
	//
	// After adding all frames, if we don't need to send an ACK frame and have not
	// added any other frames, we abandon the packet.
	if c.appendAckFrame(now, space) {
		defer func() {
			// All frames other than ACK and PADDING are ack-eliciting,
			// so if the packet is ack-eliciting we've added additional
			// frames to it.
			if shouldSendAck || c.w.sent.ackEliciting {
				// Either we are willing to send an ACK-only packet,
				// or we've added additional frames.
				c.acks[space].sentAck()
			} else {
				// There's nothing in this packet but ACK frames, and
				// we don't want to send an ACK-only packet at this time.
				// Abandoning the packet means we wrote an ACK frame for
				// nothing, but constructing the frame is cheap.
				c.w.abandonPacket()
			}
		}()
	}
	if limit != ccOK {
		return
	}
	pto := c.loss.ptoExpired

	// TODO: Add all the other frames we can send.

	// HANDSHAKE_DONE
	if c.handshakeConfirmed.shouldSendPTO(pto) {
		if !c.w.appendHandshakeDoneFrame() {
			return
		}
		c.handshakeConfirmed.setSent(pnum)
	}

	// CRYPTO
	c.crypto[space].dataToSend(pto, func(off, size int64) int64 {
		b, _ := c.w.appendCryptoFrame(off, int(size))
		c.crypto[space].sendData(off, b)
		return int64(len(b))
	})

	// NEW_CONNECTION_ID, RETIRE_CONNECTION_ID
	if space == appDataSpace {
		if !c.connIDState.appendFrames(&c.w, pnum, pto) {
			return
		}
	}

	// Test-only PING frames.
	if space == c.testSendPingSpace && c.testSendPing.shouldSendPTO(pto) {
		if !c.w.appendPingFrame() {
			return
		}
		c.testSendPing.setSent(pnum)
	}

	// All stream-related frames. This should come last in the packet,
	// so large amounts of STREAM data don't crowd out other frames
	// we may need to send.
	if !c.appendStreamFrames(&c.w, pnum, pto) {
		return
	}

	// If this is a PTO probe and we haven't added an ack-eliciting frame yet,
	// add a PING to make this an ack-eliciting probe.
	//
	// Technically, there are separate PTO timers for each number space.
	// When a PTO timer expires, we MUST send an ack-eliciting packet in the
	// timer's space. We SHOULD send ack-eliciting packets in every other space
	// with in-flight data. (RFC 9002, section 6.2.4)
	//
	// What we actually do is send a single datagram containing an ack-eliciting packet
	// for every space for which we have keys.
	//
	// We fill the PTO probe packets with new or unacknowledged data. For example,
	// a PTO probe sent for the Initial space will generally retransmit previously
	// sent but unacknowledged CRYPTO data.
	//
	// When sending a PTO probe datagram containing multiple packets, it is
	// possible that an earlier packet will fill up the datagram, leaving no
	// space for the remaining probe packet(s). This is not a problem in practice.
	//
	// A client discards Initial keys when it first sends a Handshake packet
	// (RFC 9001 Section 4.9.1). Handshake keys are discarded when the handshake
	// is confirmed (RFC 9001 Section  4.9.2). The PTO timer is not set for the
	// Application Data packet number space until the handshake is confirmed
	// (RFC 9002 Section 6.2.1). Therefore, the only times a PTO probe can fire
	// while data for multiple spaces is in flight are:
	//
	// - a server's Initial or Handshake timers can fire while Initial and Handshake
	//   data is in flight; and
	//
	// - a client's Handshake timer can fire while Handshake and Application Data
	//   data is in flight.
	//
	// It is theoretically possible for a server's Initial CRYPTO data to overflow
	// the maximum datagram size, but unlikely in practice; this space contains
	// only the ServerHello TLS message, which is small. It's also unlikely that
	// the Handshake PTO probe will fire while Initial data is in flight (this
	// requires not just that the Initial CRYPTO data completely fill a datagram,
	// but a quite specific arrangement of lost and retransmitted packets.)
	// We don't bother worrying about this case here, since the worst case is
	// that we send a PTO probe for the in-flight Initial data and drop the
	// Handshake probe.
	//
	// If a client's Handshake PTO timer fires while Application Data data is in
	// flight, it is possible that the resent Handshake CRYPTO data will crowd
	// out the probe for the Application Data space. However, since this probe is
	// optional (recall that the Application Data PTO timer is never set until
	// after Handshake keys have been discarded), dropping it is acceptable.
	if pto && !c.w.sent.ackEliciting {
		c.w.appendPingFrame()
	}
}

func (c *Conn) appendAckFrame(now time.Time, space numberSpace) bool {
	seen, delay := c.acks[space].acksToSend(now)
	if len(seen) == 0 {
		return false
	}
	d := unscaledAckDelayFromDuration(delay, ackDelayExponent)
	return c.w.appendAckFrame(seen, d)
}

func (c *Conn) appendConnectionCloseFrame(err error) {
	// TODO: Send application errors.
	switch e := err.(type) {
	case localTransportError:
		c.w.appendConnectionCloseTransportFrame(transportError(e), 0, "")
	default:
		// TLS alerts are sent using error codes [0x0100,0x01ff).
		// https://www.rfc-editor.org/rfc/rfc9000#section-20.1-2.36.1
		var alert tls.AlertError
		if errors.As(err, &alert) {
			// tls.AlertError is a uint8, so this can't exceed 0x01ff.
			code := errTLSBase + transportError(alert)
			c.w.appendConnectionCloseTransportFrame(code, 0, "")
			return
		}
		c.w.appendConnectionCloseTransportFrame(errInternal, 0, "")
	}
}
