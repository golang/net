// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package context defines the Context type, which carries deadlines, cancellation
// signals, and other request-scoped values across API boundaries and between
// processes.
//
// Incoming requests to a server establish a Context, and outgoing calls to servers
// should accept a Context.  The chain of function calls between must propagate the
// Context, optionally replacing it with a modified copy created using
// WithDeadline, WithTimeout, WithCancel, or WithValue.
//
// Programs that use Contexts should follow these rules to keep interfaces
// consistent across packages and enable static analysis tools to check context
// propagation:
//
// Do not store Contexts inside a struct type; instead, pass a Context
// explicitly to each function that needs it.  The Context should be the first
// parameter, typically named ctx:
//
// 	func DoSomething(ctx context.Context, arg Arg) error {
// 		// ... use ctx ...
// 	}
//
// Do not pass a nil Context, even if a function permits it.  Pass context.TODO
// if you are unsure about which Context to use.
//
// Use context Values only for request-scoped data that transits processes and
// APIs, not for passing optional parameters to functions.
//
// The same Context may be passed to functions running in different goroutines;
// Contexts are safe for simultaneous use by multiple goroutines.
package context

import (
	"errors"
	"sync"
	"time"
)

// A Context carries deadlines, and cancellation signals, and other values
// across API boundaries.
//
// Context's methods may be called by multiple goroutines simultaneously.
type Context interface {
	// Deadline returns the time when work done on behalf of this context
	// should be canceled.  Deadline returns ok==false when no deadline is
	// set.  Successive calls to Deadline return the same results.
	Deadline() (deadline time.Time, ok bool)

	// Done returns a channel that's closed when work done on behalf of this
	// context should be canceled.  Done may return nil if this context can
	// never become done.  Successive calls to Done return the same value.
	//
	// WithCancel arranges for Done to be closed when cancel is called;
	// WithDeadline arranges for Done to be closed when the deadline
	// expires; WithTimeout arranges for Done to be closed when the timeout
	// elapses.
	//
	// Done is provided for use in select statements:
	//
	// 	// DoSomething calls DoSomethingSlow and returns as soon as
	// 	// it returns or ctx.Done is closed.
	// 	func DoSomething(ctx context.Context) (Result, error) {
	// 		c := make(chan Result, 1)
	// 		go func() { c <- DoSomethingSlow(ctx) }()
	// 		select {
	// 		case res := <-c:
	// 			return res, nil
	// 		case <-ctx.Done():
	// 			return nil, ctx.Err()
	// 		}
	// 	}
	Done() <-chan struct{}

	// Err returns a non-nil error value after Done is closed.  Err returns
	// Canceled if the context was canceled; Err returns DeadlineExceeded if
	// the context's deadline passed.  No other values for Err are defined.
	// After Done is closed, successive calls to Err return the same value.
	Err() error

	// Value returns the value associated with this context for key, or nil
	// if no value is associated with key.  Successive calls to Value with
	// the same key returns the same result.
	//
	// Use context values only for request-scoped data that transits
	// processes and APIs, not for passing optional parameters to functions.
	//
	// A key identifies a specific value in a Context.  Functions that wish
	// to store values in Context typically allocate a key in a global
	// variable then use that key as the argument to context.WithValue and
	// Context.Value.  A key can be any type that supports equality;
	// packages should define keys as an unexported type to avoid
	// collisions.
	//
	// Packages that define a Context key should provide type-safe accessors
	// for the values stores using that key:
	//
	// 	// Package user defines a User type that's stored in Contexts.
	// 	package user
	//
	// 	import "code.google.com/p/go.net/context"
	//
	// 	// User is the type of value stored in the Contexts.
	// 	type User struct {...}
	//
	// 	// key is an unexported type for keys defined in this package.
	// 	// This prevents collisions with keys defined in other packages.
	// 	type key int
	//
	// 	// userKey is the key for user.User values in Contexts.  It is
	// 	// unexported; clients use user.NewContext and user.FromContext
	// 	// instead of using this key directly.
	// 	var userKey key = 0
	//
	// 	// NewContext returns a new Context that carries value u.
	// 	func NewContext(ctx context.Context, u *User) context.Context {
	// 		return context.WithValue(userKey, u)
	// 	}
	//
	// 	// FromContext returns the User value stored in ctx, if any.
	// 	func FromContext(ctx context.Context) (*User, bool) {
	// 		u, ok := ctx.Value(userKey).(*User)
	// 		return u, ok
	// 	}
	Value(key interface{}) interface{}
}

// Canceled is the error returned by Context.Err when the context is canceled.
var Canceled = errors.New("context canceled")

// DeadlineExceeded is the error returned by Context.Err when the context's
// deadline passes.
var DeadlineExceeded = errors.New("context deadline exceeded")

// A ctx is a Context that automatically propagates cancellation signals to
// other ctxs (those created using this ctx as their parent).  A ctx also
// manages its own deadline timer.
//
// TODO(sameer): split this into separate concrete types for WithValue,
// WithCancel, and WithTimeout/Deadline.  This reduces the size of the structs;
// for example, we don't need a timer field when creating a Context using
// WithValue.
type ctx struct {
	parent Context       // set by newCtx
	done   chan struct{} // closed by the first cancel call.  nil if uncancelable.

	key interface{} // set by WithValue
	val interface{} // set by WithValue

	deadline    time.Time // set by WithDeadline
	deadlineSet bool      // set by WithDeadline

	// parent.mu ACQUIRED_BEFORE mu: mu must not be held when acquiring parent.mu.
	mu       sync.RWMutex
	children map[*ctx]bool // set to nil by the first cancel call
	err      error         // set to non-nil by the first cancel call
	timer    *time.Timer   // set by WithDeadline, read by cancel
}

func (c *ctx) Deadline() (deadline time.Time, ok bool) {
	return c.deadline, c.deadlineSet
}

func (c *ctx) Done() <-chan struct{} {
	return c.done // may be nil
}

func (c *ctx) Err() error {
	c.mu.RLock() // c.err is under mu
	defer c.mu.RUnlock()
	return c.err
}

func (c *ctx) Value(key interface{}) interface{} {
	if c.key == key {
		return c.val
	}
	if c.parent != nil {
		return c.parent.Value(key)
	}
	return nil
}

// The background context for this process.
var background = newCtx(nil, neverCanceled)

// Background returns a non-nil, empty Context. It is never canceled, has no
// values, and has no deadline.  It is typically used by the main function,
// initialization, and tests, and as the top-level Context for incoming
// requests.
func Background() Context {
	return background
}

// TODO returns a non-nil, empty Context.  Code should use context.TODO when
// it's unclear which Context to use or it's is not yet available (because the
// surrounding function has not yet been extended to accept a Context
// parameter).  TODO is recognized by static analysis tools that determine
// whether Contexts are propagated correctly in a program.
func TODO() Context {
	return Background()
}

// A CancelFunc tells an operation to abandon its work.
// A CancelFunc does not wait for the work to stop.
// After the first, subsequent calls to a CancelFunc do nothing.
type CancelFunc func()

// WithCancel returns a copy of parent with a new Done channel. The returned
// context's Done channel is closed when the returned cancel function is called
// or when the parent context's Done channel is closed, whichever happens first.
func WithCancel(parent Context) (ctx Context, cancel CancelFunc) {
	return withCancel(parent)
}

func withCancel(parent Context) (*ctx, CancelFunc) {
	c := newCtx(parent, maybeCanceled)
	return c, func() { c.cancel(true, Canceled) }
}

// WithDeadline returns a copy of the parent context with the deadline adjusted
// to be no later than d.  If the parent's deadline is already earlier than d,
// WithDeadline(parent, d) is semantically equivalent to parent.  The returned
// context's Done channel is closed when the deadline expires, when the returned
// cancel function is called, or when the parent context's Done channel is
// closed, whichever happens first.
//
// Canceling this context releases resources associated with the deadline
// timer, so code should call cancel as soon as the operations running in this
// Context complete.
func WithDeadline(parent Context, deadline time.Time) (Context, CancelFunc) {
	c, cancel := withCancel(parent)
	if cur, ok := c.Deadline(); ok && cur.Before(deadline) {
		// The current deadline is already sooner than the new one.
		return c, cancel
	}
	c.deadline, c.deadlineSet = deadline, true
	d := deadline.Sub(time.Now())
	if d <= 0 {
		// TODO(sameer): pass removeFromParent=true here?
		c.cancel(false, DeadlineExceeded) // deadline has already passed
		return c, cancel
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timer = time.AfterFunc(d, func() {
		// TODO(sameer): pass removeFromParent=true here?
		c.cancel(false, DeadlineExceeded)
	})
	return c, cancel
}

// WithTimeout returns WithDeadline(parent, time.Now().Add(timeout)).
//
// Canceling this context releases resources associated with the deadline
// timer, so code should call cancel as soon as the operations running in this
// Context complete:
//
// 	func slowOperationWithTimeout(ctx context.Context) (Result, error) {
// 		ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
// 		defer cancel()  // releases resources if slowOperation completes before timeout elapses
// 		return slowOperation(ctx)
// 	}
func WithTimeout(parent Context, timeout time.Duration) (Context, CancelFunc) {
	return WithDeadline(parent, time.Now().Add(timeout))
}

// WithValue returns a copy of parent in which the value associated with key is
// val.
//
// Use context Values only for request-scoped data that transits processes and
// APIs, not for passing optional parameters to functions.
func WithValue(parent Context, key interface{}, val interface{}) Context {
	c := newCtx(parent, neverCanceled)
	c.key, c.val = key, val
	return c
}

const maybeCanceled = true
const neverCanceled = false

func newCtx(parent Context, childMayCancel bool) *ctx {
	c := &ctx{parent: parent}
	parentMayCancel := parent != nil && parent.Done() != nil
	if childMayCancel || parentMayCancel {
		c.done = make(chan struct{})
	}
	if parent != nil {
		c.deadline, c.deadlineSet = parent.Deadline()
	}
	if parentMayCancel {
		if p, ok := parent.(*ctx); ok {
			// Arrange for the new ctx to be canceled when the parent is.
			p.mu.Lock()
			if p.err != nil {
				// parent has already been canceled
				c.cancel(false, p.err)
			} else {
				if p.children == nil {
					p.children = make(map[*ctx]bool)
				}
				p.children[c] = true
			}
			p.mu.Unlock()
		} else {
			// Cancel the new ctx when context.Done is closed.
			go func() {
				select {
				case <-parent.Done():
					c.cancel(false, parent.Err())
				case <-c.done:
				}
			}()
		}
	}
	return c
}

// cancel closes c.done, cancels each of this ctx's children, and, if
// removeFromParent is true, removes this ctx from its parent's children.
// cancel stops c.timer, if it is running.
func (c *ctx) cancel(removeFromParent bool, err error) {
	if err == nil {
		panic("context: internal error: missing cancel error")
	}
	c.mu.Lock()
	if c.done == nil {
		panic("context: internal error: missing done channel")
	}
	if c.err != nil {
		c.mu.Unlock()
		return // already canceled
	}
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	close(c.done)
	c.err = err
	for child := range c.children {
		// NOTE: acquiring the child's lock while holding parent's lock.
		child.cancel(false, err)
	}
	c.children = nil
	c.mu.Unlock()

	if p, ok := c.parent.(*ctx); ok && p != nil && removeFromParent {
		p.mu.Lock()
		if p.children != nil {
			delete(p.children, c)
		}
		p.mu.Unlock()
	}
}
