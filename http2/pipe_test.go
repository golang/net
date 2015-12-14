// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestPipeClose(t *testing.T) {
	var p pipe
	p.b = new(bytes.Buffer)
	a := errors.New("a")
	b := errors.New("b")
	p.CloseWithError(a)
	p.CloseWithError(b)
	_, err := p.Read(make([]byte, 1))
	if err != a {
		t.Errorf("err = %v want %v", err, a)
	}
}

func TestPipeDoneChan(t *testing.T) {
	var p pipe
	done := p.Done()
	select {
	case <-done:
		t.Fatal("done too soon")
	default:
	}
	p.CloseWithError(io.EOF)
	select {
	case <-done:
	default:
		t.Fatal("should be done")
	}
}

func TestPipeDoneChan_ErrFirst(t *testing.T) {
	var p pipe
	p.CloseWithError(io.EOF)
	done := p.Done()
	select {
	case <-done:
	default:
		t.Fatal("should be done")
	}
}
