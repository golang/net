// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"errors"
	"fmt"
	"time"
)

// A Conn is a QUIC connection.
//
// Multiple goroutines may invoke methods on a Conn simultaneously.
type Conn struct {
	msgc   chan any
	donec  chan struct{} // closed when conn loop exits
	exited bool          // set to make the conn loop exit immediately

	testHooks connTestHooks

	// idleTimeout is the time at which the connection will be closed due to inactivity.
	// https://www.rfc-editor.org/rfc/rfc9000#section-10.1
	maxIdleTimeout time.Duration
	idleTimeout    time.Time
}

// connTestHooks override conn behavior in tests.
type connTestHooks interface {
	nextMessage(msgc chan any, nextTimeout time.Time) (now time.Time, message any)
}

func newConn(now time.Time, hooks connTestHooks) (*Conn, error) {
	c := &Conn{
		donec:          make(chan struct{}),
		testHooks:      hooks,
		maxIdleTimeout: defaultMaxIdleTimeout,
		idleTimeout:    now.Add(defaultMaxIdleTimeout),
	}

	// A one-element buffer allows us to wake a Conn's event loop as a
	// non-blocking operation.
	c.msgc = make(chan any, 1)

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
		nextTimeout := c.idleTimeout

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
		case timerEvent:
			// A connection timer has expired.
			if !now.Before(c.idleTimeout) {
				// "[...] the connection is silently closed and
				// its state is discarded [...]"
				// https://www.rfc-editor.org/rfc/rfc9000#section-10.1-1
				c.exited = true
				return
			}
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
func (c *Conn) sendMsg(m any) error {
	select {
	case c.msgc <- m:
	case <-c.donec:
		return errors.New("quic: connection closed")
	}
	return nil
}

// runOnLoop executes a function within the conn's loop goroutine.
func (c *Conn) runOnLoop(f func(now time.Time, c *Conn)) error {
	donec := make(chan struct{})
	if err := c.sendMsg(func(now time.Time, c *Conn) {
		defer close(donec)
		f(now, c)
	}); err != nil {
		return err
	}
	select {
	case <-donec:
	case <-c.donec:
		return errors.New("quic: connection closed")
	}
	return nil
}

// exit fully terminates a connection immediately.
func (c *Conn) exit() {
	c.runOnLoop(func(now time.Time, c *Conn) {
		c.exited = true
	})
	<-c.donec
}
