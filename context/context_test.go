// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package context

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// otherContext is a Context that's not a *ctx.  This lets us test code paths
// that differ based on the underlying type of the Context.
type otherContext struct {
	Context
}

func TestBackground(t *testing.T) {
	c := Background()
	if c == nil {
		t.Fatalf("Background returned nil")
	}
	select {
	case x := <-c.Done():
		t.Errorf("<-c.Done() == %v want nothing (it should block)", x)
	default:
	}
}

func TestWithCancel(t *testing.T) {
	c1, cancel := WithCancel(Background())
	o := otherContext{c1}
	c2 := newCtx(o, maybeCanceled)
	contexts := []Context{c1, o, c2}

	for i, c := range contexts {
		if d := c.Done(); d == nil {
			t.Errorf("c[%d].Done() == %v want non-nil", i, d)
		}
		if e := c.Err(); e != nil {
			t.Errorf("c[%d].Err() == %v want nil", i, e)
		}

		select {
		case x := <-c.Done():
			t.Errorf("<-c.Done() == %v want nothing (it should block)", x)
		default:
		}
	}

	cancel()
	time.Sleep(100 * time.Millisecond) // let cancellation propagate

	for i, c := range contexts {
		select {
		case <-c.Done():
		default:
			t.Errorf("<-c[%d].Done() blocked, but shouldn't have", i)
		}
		if e := c.Err(); e != Canceled {
			t.Errorf("c[%d].Err() == %v want %v", i, e, Canceled)
		}
	}
}

func TestParentFinishesChild(t *testing.T) {
	parent, cancel := WithCancel(Background())
	pctx := parent.(*ctx)
	child1 := newCtx(parent, maybeCanceled)
	child2 := newCtx(parent, neverCanceled)

	select {
	case x := <-parent.Done():
		t.Errorf("<-parent.Done() == %v want nothing (it should block)", x)
	case x := <-child1.Done():
		t.Errorf("<-child1.Done() == %v want nothing (it should block)", x)
	case x := <-child2.Done():
		t.Errorf("<-child2.Done() == %v want nothing (it should block)", x)
	default:
	}

	pctx.mu.Lock()
	if len(pctx.children) != 2 ||
		!pctx.children[child1] || child1.parent != pctx ||
		!pctx.children[child2] || child2.parent != pctx {
		t.Errorf("bad linkage: pctx.children = %v, child1.parent = %v, child2.parent = %v",
			pctx.children, child1.parent, child2.parent)
	}
	pctx.mu.Unlock()

	cancel()

	pctx.mu.Lock()
	if len(pctx.children) != 0 {
		t.Errorf("pctx.cancel didn't clear pctx.children = %v", pctx.children)
	}
	pctx.mu.Unlock()

	// parent and children should all be finished.
	select {
	case <-parent.Done():
	default:
		t.Errorf("<-parent.Done() blocked, but shouldn't have")
	}
	if e := parent.Err(); e != Canceled {
		t.Errorf("parent.Err() == %v want %v", e, Canceled)
	}
	select {
	case <-child1.Done():
	default:
		t.Errorf("<-child1.Done() blocked, but shouldn't have")
	}
	if e := child1.Err(); e != Canceled {
		t.Errorf("child1.Err() == %v want %v", e, Canceled)
	}
	select {
	case <-child2.Done():
	default:
		t.Errorf("<-child2.Done() blocked, but shouldn't have")
	}
	if e := child2.Err(); e != Canceled {
		t.Errorf("child2.Err() == %v want %v", e, Canceled)
	}

	// New should return a canceled context on a canceled parent.
	child3 := newCtx(parent, neverCanceled)
	select {
	case <-child3.Done():
	default:
		t.Errorf("<-child3.Done() blocked, but shouldn't have")
	}
	if e := child3.Err(); e != Canceled {
		t.Errorf("child3.Err() == %v want %v", e, Canceled)
	}
}

func TestChildFinishesFirst(t *testing.T) {
	for _, parentMayCancel := range []bool{neverCanceled, maybeCanceled} {
		parent := newCtx(nil, parentMayCancel)
		child, cancel := WithCancel(parent)
		pctx := parent
		cctx := child.(*ctx)

		select {
		case x := <-parent.Done():
			t.Errorf("<-parent.Done() == %v want nothing (it should block)", x)
		case x := <-child.Done():
			t.Errorf("<-child.Done() == %v want nothing (it should block)", x)
		default:
		}

		if cctx.parent != pctx {
			t.Errorf("bad linkage: cctx.parent = %v, parent = %v", cctx.parent, pctx)
		}

		if parentMayCancel {
			pctx.mu.Lock()
			if len(pctx.children) != 1 || !pctx.children[cctx] {
				t.Errorf("bad linkage: pctx.children = %v, cctx = %v", pctx.children, cctx)
			}
			pctx.mu.Unlock()
		}

		cancel()

		pctx.mu.Lock()
		if len(pctx.children) != 0 {
			t.Errorf("child.Cancel didn't remove self from pctx.children = %v", pctx.children)
		}
		pctx.mu.Unlock()

		// child should be finished.
		select {
		case <-child.Done():
		default:
			t.Errorf("<-child.Done() blocked, but shouldn't have")
		}
		if e := child.Err(); e != Canceled {
			t.Errorf("child.Err() == %v want %v", e, Canceled)
		}

		// parent should not be finished.
		select {
		case x := <-parent.Done():
			t.Errorf("<-parent.Done() == %v want nothing (it should block)", x)
		default:
		}
		if e := parent.Err(); e != nil {
			t.Errorf("parent.Err() == %v want nil", e)
		}
	}
}

func testDeadline(c Context, wait time.Duration, t *testing.T) {
	select {
	case <-time.After(wait):
		t.Fatalf("context should have timed out")
	case <-c.Done():
	}
	if e := c.Err(); e != DeadlineExceeded {
		t.Errorf("c.Err() == %v want %v", e, DeadlineExceeded)
	}
}

func TestDeadline(t *testing.T) {
	c, _ := WithDeadline(nil, time.Now().Add(100*time.Millisecond))
	testDeadline(c, 200*time.Millisecond, t)

	c, _ = WithDeadline(nil, time.Now().Add(100*time.Millisecond))
	o := otherContext{c}
	testDeadline(o, 200*time.Millisecond, t)

	c, _ = WithDeadline(nil, time.Now().Add(100*time.Millisecond))
	o = otherContext{c}
	c, _ = WithDeadline(o, time.Now().Add(300*time.Millisecond))
	testDeadline(c, 200*time.Millisecond, t)
}

func TestTimeout(t *testing.T) {
	c, _ := WithTimeout(nil, 100*time.Millisecond)
	testDeadline(c, 200*time.Millisecond, t)

	c, _ = WithTimeout(nil, 100*time.Millisecond)
	o := otherContext{c}
	testDeadline(o, 200*time.Millisecond, t)

	c, _ = WithTimeout(nil, 100*time.Millisecond)
	o = otherContext{c}
	c, _ = WithTimeout(o, 300*time.Millisecond)
	testDeadline(c, 200*time.Millisecond, t)
}

func TestCancelledTimeout(t *testing.T) {
	c, _ := WithTimeout(nil, 200*time.Millisecond)
	o := otherContext{c}
	c, cancel := WithTimeout(o, 400*time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond) // let cancellation propagate
	select {
	case <-c.Done():
	default:
		t.Errorf("<-c.Done() blocked, but shouldn't have")
	}
	if e := c.Err(); e != Canceled {
		t.Errorf("c.Err() == %v want %v", e, Canceled)
	}
}

type key1 int
type key2 int

var k1 = key1(1)
var k2 = key2(1) // same int as k1, different type
var k3 = key2(3) // same type as k2, different int

func TestValues(t *testing.T) {
	check := func(c Context, nm, v1, v2, v3 string) {
		if v, ok := c.Value(k1).(string); ok == (len(v1) == 0) || v != v1 {
			t.Errorf(`%s.Value(k1).(string) = %q, %t want %q, %t`, nm, v, ok, v1, len(v1) != 0)
		}
		if v, ok := c.Value(k2).(string); ok == (len(v2) == 0) || v != v2 {
			t.Errorf(`%s.Value(k2).(string) = %q, %t want %q, %t`, nm, v, ok, v2, len(v2) != 0)
		}
		if v, ok := c.Value(k3).(string); ok == (len(v3) == 0) || v != v3 {
			t.Errorf(`%s.Value(k3).(string) = %q, %t want %q, %t`, nm, v, ok, v3, len(v3) != 0)
		}
	}

	c0 := Background()
	check(c0, "c0", "", "", "")

	c1 := WithValue(nil, k1, "c1k1")
	check(c1, "c1", "c1k1", "", "")

	c2 := WithValue(c1, k2, "c2k2")
	check(c2, "c2", "c1k1", "c2k2", "")

	c3 := WithValue(c2, k3, "c3k3")
	check(c3, "c2", "c1k1", "c2k2", "c3k3")

	c4 := WithValue(c3, k1, nil)
	check(c4, "c4", "", "c2k2", "c3k3")

	o0 := otherContext{Background()}
	check(o0, "o0", "", "", "")

	o1 := otherContext{WithValue(nil, k1, "c1k1")}
	check(o1, "o1", "c1k1", "", "")

	o2 := WithValue(o1, k2, "o2k2")
	check(o2, "o2", "c1k1", "o2k2", "")

	o3 := otherContext{c4}
	check(o3, "o3", "", "c2k2", "c3k3")

	o4 := WithValue(o3, k3, nil)
	check(o4, "o4", "", "c2k2", "")
}

func TestAllocs(t *testing.T) {
	bg := Background()
	for _, test := range []struct {
		desc       string
		f          func()
		limit      float64
		gccgoLimit float64
	}{
		{
			desc:       "Background()",
			f:          func() { Background() },
			limit:      0,
			gccgoLimit: 0,
		},
		{
			desc: fmt.Sprintf("WithValue(bg, %v, nil)", k1),
			f: func() {
				c := WithValue(bg, k1, nil)
				c.Value(k1)
			},
			limit:      3,
			gccgoLimit: 3,
		},
		{
			desc: "WithTimeout(bg, 15*time.Millisecond)",
			f: func() {
				c, _ := WithTimeout(bg, 15*time.Millisecond)
				<-c.Done()
			},
			limit:      9,
			gccgoLimit: 13,
		},
		{
			desc: "WithCancel(bg)",
			f: func() {
				c, cancel := WithCancel(bg)
				cancel()
				<-c.Done()
			},
			limit:      7,
			gccgoLimit: 8,
		},
		{
			desc: "WithTimeout(bg, 100*time.Millisecond)",
			f: func() {
				c, cancel := WithTimeout(bg, 100*time.Millisecond)
				cancel()
				<-c.Done()
			},
			limit:      16,
			gccgoLimit: 25,
		},
	} {
		limit := test.limit
		if runtime.Compiler == "gccgo" {
			// gccgo does not yet do escape analysis.
			// TOOD(iant): Remove this when gccgo does do escape analysis.
			limit = test.gccgoLimit
		}
		if n := testing.AllocsPerRun(100, test.f); n > limit {
			t.Errorf("%s allocs = %f want %d", test.desc, n, int(limit))
		}
	}
}

func TestSimultaneousCancels(t *testing.T) {
	root, cancel := WithCancel(Background())
	m := map[Context]CancelFunc{root: cancel}
	q := []Context{root}
	// Create a tree of contexts.
	for len(q) != 0 && len(m) < 100 {
		parent := q[0]
		q = q[1:]
		for i := 0; i < 4; i++ {
			ctx, cancel := WithCancel(parent)
			m[ctx] = cancel
			q = append(q, ctx)
		}
	}
	// Start all the cancels in a random order.
	var wg sync.WaitGroup
	wg.Add(len(m))
	for _, cancel := range m {
		go func(cancel CancelFunc) {
			cancel()
			wg.Done()
		}(cancel)
	}
	// Wait on all the contexts in a random order.
	for ctx := range m {
		select {
		case <-ctx.Done():
		case <-time.After(1 * time.Second):
			buf := make([]byte, 10<<10)
			n := runtime.Stack(buf, true)
			t.Fatalf("timed out waiting for <-ctx.Done(); stacks:\n%s", buf[:n])
		}
	}
	// Wait for all the cancel functions to return.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		buf := make([]byte, 10<<10)
		n := runtime.Stack(buf, true)
		t.Fatalf("timed out waiting for cancel functions; stacks:\n%s", buf[:n])
	}
}

func TestInterlockedCancels(t *testing.T) {
	parent, cancelParent := WithCancel(Background())
	child, cancelChild := WithCancel(parent)
	go func() {
		parent.Done()
		cancelChild()
	}()
	cancelParent()
	select {
	case <-child.Done():
	case <-time.After(1 * time.Second):
		buf := make([]byte, 10<<10)
		n := runtime.Stack(buf, true)
		t.Fatalf("timed out waiting for child.Done(); stacks:\n%s", buf[:n])
	}
}
