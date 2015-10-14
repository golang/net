// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"errors"
	"testing"
)

func TestPipeClose(t *testing.T) {
	var p pipe
	p.c.L = &p.m
	a := errors.New("a")
	b := errors.New("b")
	p.Close(a)
	p.Close(b)
	_, err := p.Read(make([]byte, 1))
	if err != a {
		t.Errorf("err = %v want %v", err, a)
	}
}
