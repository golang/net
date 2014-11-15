// Copyright 2014 The Go Authors.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package http2

import (
	"testing"
	"time"
)

func TestFlow(t *testing.T) {
	f := newFlow(10)
	if got, want := f.cur(), int32(10); got != want {
		t.Fatalf("size = %d; want %d", got, want)
	}
	if waits := f.acquire(1); waits != 0 {
		t.Errorf("waits = %d; want 0", waits)
	}
	if got, want := f.cur(), int32(9); got != want {
		t.Fatalf("size = %d; want %d", got, want)
	}

	// Wait for 10, which should block, so start a background goroutine
	// to refill it.
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.add(50)
	}()
	if waits := f.acquire(10); waits != 1 {
		t.Errorf("waits for 50 = %d; want 0", waits)
	}

	if got, want := f.cur(), int32(49); got != want {
		t.Fatalf("size = %d; want %d", got, want)
	}
}

func TestFlowAdd(t *testing.T) {
	f := newFlow(0)
	if !f.add(1) {
		t.Fatal("failed to add 1")
	}
	if !f.add(-1) {
		t.Fatal("failed to add -1")
	}
	if got, want := f.cur(), int32(0); got != want {
		t.Fatalf("size = %d; want %d", got, want)
	}
	if !f.add(1<<31 - 1) {
		t.Fatal("failed to add 2^31-1")
	}
	if got, want := f.cur(), int32(1<<31-1); got != want {
		t.Fatalf("size = %d; want %d", got, want)
	}
	if f.add(1) {
		t.Fatal("adding 1 to max shouldn't be allowed")
	}

}

func TestFlowClose(t *testing.T) {
	f := newFlow(0)

	// Wait for 10, which should block, so start a background goroutine
	// to refill it.
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.close()
	}()
	donec := make(chan bool)
	go func() {
		defer close(donec)
		f.acquire(10)
	}()
	select {
	case <-donec:
	case <-time.After(2 * time.Second):
		t.Error("timeout")
	}
}
