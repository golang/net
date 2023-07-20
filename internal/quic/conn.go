// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// A Conn is a QUIC connection.
//
// Multiple goroutines may invoke methods on a Conn simultaneously.
type Conn struct {
	side      connSide
	listener  connListener
	testHooks connTestHooks
	peerAddr  netip.AddrPort

	msgc   chan any
	donec  chan struct{} // closed when conn loop exits
	exited bool          // set to make the conn loop exit immediately

	w           packetWriter
	acks        [numberSpaceCount]ackState // indexed by number space
	connIDState connIDState
	tlsState    tlsState
	loss        lossState

	// idleTimeout is the time at which the connection will be closed due to inactivity.
	// https://www.rfc-editor.org/rfc/rfc9000#section-10.1
	maxIdleTimeout time.Duration
	idleTimeout    time.Time

	peerAckDelayExponent int8 // -1 when unknown

	// Tests only: Send a PING in a specific number space.
	testSendPingSpace numberSpace
	testSendPing      sentVal
}

// The connListener is the Conn's Listener.
// Defined as an interface so we can swap it out in tests.
type connListener interface {
	sendDatagram(p []byte, addr netip.AddrPort) error
}

// connTestHooks override conn behavior in tests.
type connTestHooks interface {
	nextMessage(msgc chan any, nextTimeout time.Time) (now time.Time, message any)
}

func newConn(now time.Time, side connSide, initialConnID []byte, peerAddr netip.AddrPort, l connListener, hooks connTestHooks) (*Conn, error) {
	c := &Conn{
		side:                 side,
		listener:             l,
		peerAddr:             peerAddr,
		msgc:                 make(chan any, 1),
		donec:                make(chan struct{}),
		testHooks:            hooks,
		maxIdleTimeout:       defaultMaxIdleTimeout,
		idleTimeout:          now.Add(defaultMaxIdleTimeout),
		peerAckDelayExponent: -1,
	}

	// A one-element buffer allows us to wake a Conn's event loop as a
	// non-blocking operation.
	c.msgc = make(chan any, 1)

	if c.side == clientSide {
		if err := c.connIDState.initClient(newRandomConnID); err != nil {
			return nil, err
		}
		initialConnID = c.connIDState.dstConnID()
	} else {
		if err := c.connIDState.initServer(newRandomConnID, initialConnID); err != nil {
			return nil, err
		}
	}

	// The smallest allowed maximum QUIC datagram size is 1200 bytes.
	// TODO: PMTU discovery.
	const maxDatagramSize = 1200
	c.loss.init(c.side, maxDatagramSize, now)

	c.tlsState.init(c.side, initialConnID)

	go c.loop(now)
	return c, nil
}

type timerEvent struct{}

// loop is the connection main loop.
//
// Except where otherwise noted, all connection state is owned by the loop goroutine.
//
// The loop processes messages from c.msgc and timer events.
// Other goroutines may examine or modify conn state by sending the loop funcs to execute.
func (c *Conn) loop(now time.Time) {
	defer close(c.donec)

	// The connection timer sends a message to the connection loop on expiry.
	// We need to give it an expiry when creating it, so set the initial timeout to
	// an arbitrary large value. The timer will be reset before this expires (and it
	// isn't a problem if it does anyway). Skip creating the timer in tests which
	// take control of the connection message loop.
	var timer *time.Timer
	var lastTimeout time.Time
	hooks := c.testHooks
	if hooks == nil {
		timer = time.AfterFunc(1*time.Hour, func() {
			c.sendMsg(timerEvent{})
		})
		defer timer.Stop()
	}

	for !c.exited {
		sendTimeout := c.maybeSend(now) // try sending

		// Note that we only need to consider the ack timer for the App Data space,
		// since the Initial and Handshake spaces always ack immediately.
		nextTimeout := sendTimeout
		nextTimeout = firstTime(nextTimeout, c.idleTimeout)
		nextTimeout = firstTime(nextTimeout, c.loss.timer)
		nextTimeout = firstTime(nextTimeout, c.acks[appDataSpace].nextAck)

		var m any
		if hooks != nil {
			// Tests only: Wait for the test to tell us to continue.
			now, m = hooks.nextMessage(c.msgc, nextTimeout)
		} else if !nextTimeout.IsZero() && nextTimeout.Before(now) {
			// A connection timer has expired.
			now = time.Now()
			m = timerEvent{}
		} else {
			// Reschedule the connection timer if necessary
			// and wait for the next event.
			if !nextTimeout.Equal(lastTimeout) && !nextTimeout.IsZero() {
				// Resetting a timer created with time.AfterFunc guarantees
				// that the timer will run again. We might generate a spurious
				// timer event under some circumstances, but that's okay.
				timer.Reset(nextTimeout.Sub(now))
				lastTimeout = nextTimeout
			}
			m = <-c.msgc
			now = time.Now()
		}
		switch m := m.(type) {
		case *datagram:
			c.handleDatagram(now, m)
			m.recycle()
		case timerEvent:
			// A connection timer has expired.
			if !now.Before(c.idleTimeout) {
				// "[...] the connection is silently closed and
				// its state is discarded [...]"
				// https://www.rfc-editor.org/rfc/rfc9000#section-10.1-1
				c.exited = true
				return
			}
			c.loss.advance(now, c.handleAckOrLoss)
		case func(time.Time, *Conn):
			// Send a func to msgc to run it on the main Conn goroutine
			m(now, c)
		default:
			panic(fmt.Sprintf("quic: unrecognized conn message %T", m))
		}
	}
}

// sendMsg sends a message to the conn's loop.
// It does not wait for the message to be processed.
// The conn may close before processing the message, in which case it is lost.
func (c *Conn) sendMsg(m any) {
	select {
	case c.msgc <- m:
	case <-c.donec:
	}
}

// runOnLoop executes a function within the conn's loop goroutine.
func (c *Conn) runOnLoop(f func(now time.Time, c *Conn)) error {
	donec := make(chan struct{})
	c.sendMsg(func(now time.Time, c *Conn) {
		defer close(donec)
		f(now, c)
	})
	select {
	case <-donec:
	case <-c.donec:
		return errors.New("quic: connection closed")
	}
	return nil
}

// abort terminates a connection with an error.
func (c *Conn) abort(now time.Time, err error) {
	// TODO: Send CONNECTION_CLOSE frames.
	c.exit()
}

// exit fully terminates a connection immediately.
func (c *Conn) exit() {
	c.runOnLoop(func(now time.Time, c *Conn) {
		c.exited = true
	})
	<-c.donec
}

// firstTime returns the earliest non-zero time, or zero if both times are zero.
func firstTime(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case a.Before(b):
		return a
	default:
		return b
	}
}
