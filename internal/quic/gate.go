// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import "context"

// An gate is a monitor (mutex + condition variable) with one bit of state.
//
// The condition may be either set or unset.
// Lock operations may be unconditional, or wait for the condition to be set.
// Unlock operations record the new state of the condition.
type gate struct {
	// When unlocked, exactly one of set or unset contains a value.
	// When locked, neither chan contains a value.
	set   chan struct{}
	unset chan struct{}
}

func newGate() gate {
	g := gate{
		set:   make(chan struct{}, 1),
		unset: make(chan struct{}, 1),
	}
	g.unset <- struct{}{}
	return g
}

// lock acquires the gate unconditionally.
// It reports whether the condition is set.
func (g *gate) lock() (set bool) {
	select {
	case <-g.set:
		return true
	case <-g.unset:
		return false
	}
}

// waitAndLock waits until the condition is set before acquiring the gate.
func (g *gate) waitAndLock() {
	<-g.set
}

// waitAndLockContext waits until the condition is set before acquiring the gate.
// If the context expires, waitAndLockContext returns an error and does not acquire the gate.
func (g *gate) waitAndLockContext(ctx context.Context) error {
	select {
	case <-g.set:
		return nil
	default:
	}
	select {
	case <-g.set:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitWithLock releases an acquired gate until the condition is set.
// The caller must have previously acquired the gate.
// Upon return from waitWithLock, the gate will still be held.
// If waitWithLock returns nil, the condition is set.
func (g *gate) waitWithLock(ctx context.Context) error {
	g.unlock(false)
	err := g.waitAndLockContext(ctx)
	if err != nil {
		if g.lock() {
			// The condition was set in between the context expiring
			// and us reacquiring the gate.
			err = nil
		}
	}
	return err
}

// lockIfSet acquires the gate if and only if the condition is set.
func (g *gate) lockIfSet() (acquired bool) {
	select {
	case <-g.set:
		return true
	default:
		return false
	}
}

// unlock sets the condition and releases the gate.
func (g *gate) unlock(set bool) {
	if set {
		g.set <- struct{}{}
	} else {
		g.unset <- struct{}{}
	}
}

// unlock sets the condition to the result of f and releases the gate.
// Useful in defers.
func (g *gate) unlockFunc(f func() bool) {
	g.unlock(f())
}
