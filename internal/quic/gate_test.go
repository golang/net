// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"context"
	"testing"
	"time"
)

func TestGateLockAndUnlock(t *testing.T) {
	g := newGate()
	if set := g.lock(); set {
		t.Errorf("g.lock() of never-locked gate: true, want false")
	}
	unlockedc := make(chan struct{})
	donec := make(chan struct{})
	go func() {
		defer close(donec)
		set := g.lock()
		select {
		case <-unlockedc:
		default:
			t.Errorf("g.lock() succeeded while gate was held")
		}
		if !set {
			t.Errorf("g.lock() of set gate: false, want true")
		}
		g.unlock(false)
	}()
	time.Sleep(1 * time.Millisecond)
	close(unlockedc)
	g.unlock(true)
	<-donec
	if set := g.lock(); set {
		t.Errorf("g.lock() of unset gate: true, want false")
	}
}

func TestGateWaitAndLock(t *testing.T) {
	g := newGate()
	set := false
	go func() {
		for i := 0; i < 3; i++ {
			g.lock()
			g.unlock(false)
			time.Sleep(1 * time.Millisecond)
		}
		g.lock()
		set = true
		g.unlock(true)
	}()
	g.waitAndLock()
	if !set {
		t.Errorf("g.waitAndLock() returned before gate was set")
	}
}

func TestGateWaitAndLockContext(t *testing.T) {
	g := newGate()
	// waitAndLockContext is canceled
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(1 * time.Millisecond)
		cancel()
	}()
	if err := g.waitAndLockContext(ctx); err != context.Canceled {
		t.Errorf("g.waitAndLockContext() = %v, want context.Canceled", err)
	}
	// waitAndLockContext succeeds
	set := false
	go func() {
		time.Sleep(1 * time.Millisecond)
		g.lock()
		set = true
		g.unlock(true)
	}()
	if err := g.waitAndLockContext(context.Background()); err != nil {
		t.Errorf("g.waitAndLockContext() = %v, want nil", err)
	}
	if !set {
		t.Errorf("g.waitAndLockContext() returned before gate was set")
	}
	g.unlock(true)
	// waitAndLockContext succeeds when the gate is set and the context is canceled
	if err := g.waitAndLockContext(ctx); err != nil {
		t.Errorf("g.waitAndLockContext() = %v, want nil", err)
	}
}

func TestGateWaitWithLock(t *testing.T) {
	g := newGate()
	// waitWithLock is canceled
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(1 * time.Millisecond)
		cancel()
	}()
	g.lock()
	if err := g.waitWithLock(ctx); err != context.Canceled {
		t.Errorf("g.waitWithLock() = %v, want context.Canceled", err)
	}
	// waitWithLock succeeds
	set := false
	go func() {
		g.lock()
		set = true
		g.unlock(true)
	}()
	time.Sleep(1 * time.Millisecond)
	if err := g.waitWithLock(context.Background()); err != nil {
		t.Errorf("g.waitWithLock() = %v, want nil", err)
	}
	if !set {
		t.Errorf("g.waitWithLock() returned before gate was set")
	}
}

func TestGateLockIfSet(t *testing.T) {
	g := newGate()
	if locked := g.lockIfSet(); locked {
		t.Errorf("g.lockIfSet() of unset gate = %v, want false", locked)
	}
	g.lock()
	g.unlock(true)
	if locked := g.lockIfSet(); !locked {
		t.Errorf("g.lockIfSet() of set gate = %v, want true", locked)
	}
}

func TestGateUnlockFunc(t *testing.T) {
	g := newGate()
	go func() {
		g.lock()
		defer g.unlockFunc(func() bool { return true })
	}()
	g.waitAndLock()
}
