// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netutil

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimitListener(t *testing.T) {
	const (
		max      = 5
		attempts = max * 2
		msg      = "bye\n"
	)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l = LimitListener(l, max)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()

		accepted := 0
		for {
			c, err := l.Accept()
			if err != nil {
				break
			}
			accepted++
			io.WriteString(c, msg)

			defer c.Close() // Leave c open until the listener is closed.
		}
		if accepted > max {
			t.Errorf("accepted %d simultaneous connections; want at most %d", accepted, max)
		}
	}()

	// connc keeps the client end of the dialed connections alive until the
	// test completes.
	connc := make(chan []net.Conn, 1)
	connc <- nil

	dialCtx, cancelDial := context.WithCancel(context.Background())
	defer cancelDial()
	dialer := &net.Dialer{}

	var served int32
	for n := attempts; n > 0; n-- {
		wg.Add(1)
		go func() {
			defer wg.Done()

			c, err := dialer.DialContext(dialCtx, l.Addr().Network(), l.Addr().String())
			if err != nil {
				t.Log(err)
				return
			}
			defer c.Close()

			// Keep this end of the connection alive until after the Listener
			// finishes.
			conns := append(<-connc, c)
			if len(conns) == max {
				go func() {
					// Give the server a bit of time to make sure it doesn't exceed its
					// limit after serving this connection, then cancel the remaining
					// Dials (if any).
					time.Sleep(10 * time.Millisecond)
					cancelDial()
					l.Close()
				}()
			}
			connc <- conns

			b := make([]byte, len(msg))
			if n, err := c.Read(b); n < len(b) {
				t.Log(err)
				return
			}
			atomic.AddInt32(&served, 1)
		}()
	}
	wg.Wait()

	conns := <-connc
	for _, c := range conns {
		c.Close()
	}
	t.Logf("with limit %d, served %d connections (of %d dialed, %d attempted)", max, served, len(conns), attempts)
	if served != max {
		t.Errorf("expected exactly %d served", max)
	}
}

type errorListener struct {
	net.Listener
}

func (errorListener) Accept() (net.Conn, error) {
	return nil, errFake
}

var errFake = errors.New("fake error from errorListener")

// This used to hang.
func TestLimitListenerError(t *testing.T) {
	const n = 2
	ll := LimitListener(errorListener{}, n)
	for i := 0; i < n+1; i++ {
		_, err := ll.Accept()
		if err != errFake {
			t.Fatalf("Accept error = %v; want errFake", err)
		}
	}
}

func TestLimitListenerClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ln = LimitListener(ln, 1)

	errCh := make(chan error)
	go func() {
		defer close(errCh)
		c, err := net.Dial(ln.Addr().Network(), ln.Addr().String())
		if err != nil {
			errCh <- err
			return
		}
		c.Close()
	}()

	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	err = <-errCh
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	// Allow the subsequent Accept to block before closing the listener.
	// (Accept should unblock and return.)
	timer := time.AfterFunc(10*time.Millisecond, func() {
		ln.Close()
	})

	c, err = ln.Accept()
	if err == nil {
		c.Close()
		t.Errorf("Unexpected successful Accept()")
	}
	if timer.Stop() {
		t.Errorf("Accept returned before listener closed: %v", err)
	}
}
