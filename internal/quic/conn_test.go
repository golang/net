// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"math"
	"testing"
	"time"
)

func TestConnTestConn(t *testing.T) {
	tc := newTestConn(t, serverSide)
	if got, want := tc.timeUntilEvent(), defaultMaxIdleTimeout; got != want {
		t.Errorf("new conn timeout=%v, want %v (max_idle_timeout)", got, want)
	}

	var ranAt time.Time
	tc.conn.runOnLoop(func(now time.Time, c *Conn) {
		ranAt = now
	})
	if !ranAt.Equal(tc.now) {
		t.Errorf("func ran on loop at %v, want %v", ranAt, tc.now)
	}
	tc.wait()

	nextTime := tc.now.Add(defaultMaxIdleTimeout / 2)
	tc.advanceTo(nextTime)
	tc.conn.runOnLoop(func(now time.Time, c *Conn) {
		ranAt = now
	})
	if !ranAt.Equal(nextTime) {
		t.Errorf("func ran on loop at %v, want %v", ranAt, nextTime)
	}
	tc.wait()

	tc.advanceToTimer()
	if err := tc.conn.sendMsg(nil); err == nil {
		t.Errorf("after advancing to idle timeout, sendMsg = nil, want error")
	}
	if !tc.conn.exited {
		t.Errorf("after advancing to idle timeout, exited = false, want true")
	}
}

// A testConn is a Conn whose external interactions (sending and receiving packets,
// setting timers) can be manipulated in tests.
type testConn struct {
	t              *testing.T
	conn           *Conn
	now            time.Time
	timer          time.Time
	timerLastFired time.Time
	idlec          chan struct{} // only accessed on the conn's loop
}

// newTestConn creates a Conn for testing.
//
// The Conn's event loop is controlled by the test,
// allowing test code to access Conn state directly
// by first ensuring the loop goroutine is idle.
func newTestConn(t *testing.T, side connSide) *testConn {
	t.Helper()
	tc := &testConn{
		t:   t,
		now: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	t.Cleanup(tc.cleanup)

	conn, err := newConn(tc.now, (*testConnHooks)(tc))
	if err != nil {
		tc.t.Fatal(err)
	}
	tc.conn = conn

	tc.wait()
	return tc
}

// advance causes time to pass.
func (tc *testConn) advance(d time.Duration) {
	tc.t.Helper()
	tc.advanceTo(tc.now.Add(d))
}

// advanceTo sets the current time.
func (tc *testConn) advanceTo(now time.Time) {
	tc.t.Helper()
	if tc.now.After(now) {
		tc.t.Fatalf("time moved backwards: %v -> %v", tc.now, now)
	}
	tc.now = now
	if tc.timer.After(tc.now) {
		return
	}
	tc.conn.sendMsg(timerEvent{})
	tc.wait()
}

// advanceToTimer sets the current time to the time of the Conn's next timer event.
func (tc *testConn) advanceToTimer() {
	if tc.timer.IsZero() {
		tc.t.Fatalf("advancing to timer, but timer is not set")
	}
	tc.advanceTo(tc.timer)
}

const infiniteDuration = time.Duration(math.MaxInt64)

// timeUntilEvent returns the amount of time until the next connection event.
func (tc *testConn) timeUntilEvent() time.Duration {
	if tc.timer.IsZero() {
		return infiniteDuration
	}
	if tc.timer.Before(tc.now) {
		return 0
	}
	return tc.timer.Sub(tc.now)
}

// wait blocks until the conn becomes idle.
// The conn is idle when it is blocked waiting for a packet to arrive or a timer to expire.
// Tests shouldn't need to call wait directly.
// testConn methods that wake the Conn event loop will call wait for them.
func (tc *testConn) wait() {
	tc.t.Helper()
	idlec := make(chan struct{})
	fail := false
	tc.conn.sendMsg(func(now time.Time, c *Conn) {
		if tc.idlec != nil {
			tc.t.Errorf("testConn.wait called concurrently")
			fail = true
			close(idlec)
		} else {
			// nextMessage will close idlec.
			tc.idlec = idlec
		}
	})
	select {
	case <-idlec:
	case <-tc.conn.donec:
	}
	if fail {
		panic(fail)
	}
}

func (tc *testConn) cleanup() {
	if tc.conn == nil {
		return
	}
	tc.conn.exit()
}

// testConnHooks implements connTestHooks.
type testConnHooks testConn

// nextMessage is called by the Conn's event loop to request its next event.
func (tc *testConnHooks) nextMessage(msgc chan any, timer time.Time) (now time.Time, m any) {
	tc.timer = timer
	if !timer.IsZero() && !timer.After(tc.now) {
		if timer.Equal(tc.timerLastFired) {
			// If the connection timer fires at time T, the Conn should take some
			// action to advance the timer into the future. If the Conn reschedules
			// the timer for the same time, it isn't making progress and we have a bug.
			tc.t.Errorf("connection timer spinning; now=%v timer=%v", tc.now, timer)
		} else {
			tc.timerLastFired = timer
			return tc.now, timerEvent{}
		}
	}
	select {
	case m := <-msgc:
		return tc.now, m
	default:
	}
	// If the message queue is empty, then the conn is idle.
	if tc.idlec != nil {
		idlec := tc.idlec
		tc.idlec = nil
		close(idlec)
	}
	m = <-msgc
	return tc.now, m
}
