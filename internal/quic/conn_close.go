// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"context"
	"errors"
	"time"
)

// lifetimeState tracks the state of a connection.
//
// This is fairly coupled to the rest of a Conn, but putting it in a struct of its own helps
// reason about operations that cause state transitions.
type lifetimeState struct {
	readyc    chan struct{} // closed when TLS handshake completes
	drainingc chan struct{} // closed when entering the draining state

	// Possible states for the connection:
	//
	// Alive: localErr and finalErr are both nil.
	//
	// Closing: localErr is non-nil and finalErr is nil.
	// We have sent a CONNECTION_CLOSE to the peer or are about to
	// (if connCloseSentTime is zero) and are waiting for the peer to respond.
	// drainEndTime is set to the time the closing state ends.
	// https://www.rfc-editor.org/rfc/rfc9000.html#section-10.2.1
	//
	// Draining: finalErr is non-nil.
	// If localErr is nil, we're waiting for the user to provide us with a final status
	// to send to the peer.
	// Otherwise, we've either sent a CONNECTION_CLOSE to the peer or are about to
	// (if connCloseSentTime is zero).
	// drainEndTime is set to the time the draining state ends.
	// https://www.rfc-editor.org/rfc/rfc9000.html#section-10.2.2
	localErr error // error sent to the peer
	finalErr error // error sent by the peer, or transport error; always set before draining

	connCloseSentTime time.Time     // send time of last CONNECTION_CLOSE frame
	connCloseDelay    time.Duration // delay until next CONNECTION_CLOSE frame sent
	drainEndTime      time.Time     // time the connection exits the draining state
}

func (c *Conn) lifetimeInit() {
	c.lifetime.readyc = make(chan struct{})
	c.lifetime.drainingc = make(chan struct{})
}

var errNoPeerResponse = errors.New("peer did not respond to CONNECTION_CLOSE")

// advance is called when time passes.
func (c *Conn) lifetimeAdvance(now time.Time) (done bool) {
	if c.lifetime.drainEndTime.IsZero() || c.lifetime.drainEndTime.After(now) {
		return false
	}
	// The connection drain period has ended, and we can shut down.
	// https://www.rfc-editor.org/rfc/rfc9000.html#section-10.2-7
	c.lifetime.drainEndTime = time.Time{}
	if c.lifetime.finalErr == nil {
		// The peer never responded to our CONNECTION_CLOSE.
		c.enterDraining(errNoPeerResponse)
	}
	return true
}

// confirmHandshake is called when the TLS handshake completes.
func (c *Conn) handshakeDone() {
	close(c.lifetime.readyc)
}

// isDraining reports whether the conn is in the draining state.
//
// The draining state is entered once an endpoint receives a CONNECTION_CLOSE frame.
// The endpoint will no longer send any packets, but we retain knowledge of the connection
// until the end of the drain period to ensure we discard packets for the connection
// rather than treating them as starting a new connection.
//
// https://www.rfc-editor.org/rfc/rfc9000.html#section-10.2.2
func (c *Conn) isDraining() bool {
	return c.lifetime.finalErr != nil
}

// isClosingOrDraining reports whether the conn is in the closing or draining states.
func (c *Conn) isClosingOrDraining() bool {
	return c.lifetime.localErr != nil || c.lifetime.finalErr != nil
}

// sendOK reports whether the conn can send frames at this time.
func (c *Conn) sendOK(now time.Time) bool {
	if !c.isClosingOrDraining() {
		return true
	}
	// We are closing or draining.
	if c.lifetime.localErr == nil {
		// We're waiting for the user to close the connection, providing us with
		// a final status to send to the peer.
		return false
	}
	// Past this point, returning true will result in the conn sending a CONNECTION_CLOSE
	// due to localErr being set.
	if c.lifetime.drainEndTime.IsZero() {
		// The closing and draining states should last for at least three times
		// the current PTO interval. We currently use exactly that minimum.
		// https://www.rfc-editor.org/rfc/rfc9000.html#section-10.2-5
		//
		// The drain period begins when we send or receive a CONNECTION_CLOSE,
		// whichever comes first.
		// https://www.rfc-editor.org/rfc/rfc9000.html#section-10.2.2-3
		c.lifetime.drainEndTime = now.Add(3 * c.loss.ptoBasePeriod())
	}
	if c.lifetime.connCloseSentTime.IsZero() {
		// We haven't sent a CONNECTION_CLOSE yet. Do so.
		// Either we're initiating an immediate close
		// (and will enter the closing state as soon as we send CONNECTION_CLOSE),
		// or we've read a CONNECTION_CLOSE from our peer
		// (and may send one CONNECTION_CLOSE before entering the draining state).
		//
		// Set the initial delay before we will send another CONNECTION_CLOSE.
		//
		// RFC 9000 states that we should rate limit CONNECTION_CLOSE frames,
		// but leaves the implementation of the limit up to us. Here, we start
		// with the same delay as the PTO timer (RFC 9002, Section 6.2.1),
		// not including max_ack_delay, and double it on every CONNECTION_CLOSE sent.
		c.lifetime.connCloseDelay = c.loss.rtt.smoothedRTT + max(4*c.loss.rtt.rttvar, timerGranularity)
		c.lifetime.drainEndTime = now.Add(3 * c.loss.ptoBasePeriod())
		return true
	}
	if c.isDraining() {
		// We are in the draining state, and will send no more packets.
		return false
	}
	maxRecvTime := c.acks[initialSpace].maxRecvTime
	if t := c.acks[handshakeSpace].maxRecvTime; t.After(maxRecvTime) {
		maxRecvTime = t
	}
	if t := c.acks[appDataSpace].maxRecvTime; t.After(maxRecvTime) {
		maxRecvTime = t
	}
	if maxRecvTime.Before(c.lifetime.connCloseSentTime.Add(c.lifetime.connCloseDelay)) {
		// After sending CONNECTION_CLOSE, ignore packets from the peer for
		// a delay. On the next packet received after the delay, send another
		// CONNECTION_CLOSE.
		return false
	}
	c.lifetime.connCloseSentTime = now
	c.lifetime.connCloseDelay *= 2
	return true
}

// enterDraining enters the draining state.
func (c *Conn) enterDraining(err error) {
	if c.isDraining() {
		return
	}
	if e, ok := c.lifetime.localErr.(localTransportError); ok && transportError(e) != errNo {
		// If we've terminated the connection due to a peer protocol violation,
		// record the final error on the connection as our reason for termination.
		c.lifetime.finalErr = c.lifetime.localErr
	} else {
		c.lifetime.finalErr = err
	}
	close(c.lifetime.drainingc)
	c.streams.queue.close(c.lifetime.finalErr)
}

func (c *Conn) waitReady(ctx context.Context) error {
	select {
	case <-c.lifetime.readyc:
		return nil
	case <-c.lifetime.drainingc:
		return c.lifetime.finalErr
	default:
	}
	select {
	case <-c.lifetime.readyc:
		return nil
	case <-c.lifetime.drainingc:
		return c.lifetime.finalErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close closes the connection.
//
// Close is equivalent to:
//
//	conn.Abort(nil)
//	err := conn.Wait(context.Background())
func (c *Conn) Close() error {
	c.Abort(nil)
	<-c.lifetime.drainingc
	return c.lifetime.finalErr
}

// Wait waits for the peer to close the connection.
//
// If the connection is closed locally and the peer does not close its end of the connection,
// Wait will return with a non-nil error after the drain period expires.
//
// If the peer closes the connection with a NO_ERROR transport error, Wait returns nil.
// If the peer closes the connection with an application error, Wait returns an ApplicationError
// containing the peer's error code and reason.
// If the peer closes the connection with any other status, Wait returns a non-nil error.
func (c *Conn) Wait(ctx context.Context) error {
	if err := c.waitOnDone(ctx, c.lifetime.drainingc); err != nil {
		return err
	}
	return c.lifetime.finalErr
}

// Abort closes the connection and returns immediately.
//
// If err is nil, Abort sends a transport error of NO_ERROR to the peer.
// If err is an ApplicationError, Abort sends its error code and text.
// Otherwise, Abort sends a transport error of APPLICATION_ERROR with the error's text.
func (c *Conn) Abort(err error) {
	if err == nil {
		err = localTransportError(errNo)
	}
	c.sendMsg(func(now time.Time, c *Conn) {
		c.abort(now, err)
	})
}

// abort terminates a connection with an error.
func (c *Conn) abort(now time.Time, err error) {
	if c.lifetime.localErr != nil {
		return // already closing
	}
	c.lifetime.localErr = err
}

// abortImmediately terminates a connection.
// The connection does not send a CONNECTION_CLOSE, and skips the draining period.
func (c *Conn) abortImmediately(now time.Time, err error) {
	c.abort(now, err)
	c.enterDraining(err)
	c.exited = true
}

// exit fully terminates a connection immediately.
func (c *Conn) exit() {
	c.sendMsg(func(now time.Time, c *Conn) {
		c.enterDraining(errors.New("connection closed"))
		c.exited = true
	})
}
