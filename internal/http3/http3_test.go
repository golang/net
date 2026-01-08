// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.25

package http3

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"testing/synctest"
)

func init() {
	// testing/synctest requires asynctimerchan=0 (the default as of Go 1.23),
	// but the x/net go.mod is currently selecting go1.18.
	//
	// Set asynctimerchan=0 explicitly.
	//
	// TODO: Remove this when the x/net go.mod Go version is >= go1.23.
	os.Setenv("GODEBUG", os.Getenv("GODEBUG")+",asynctimerchan=0")
}

func unhex(s string) []byte {
	b, err := hex.DecodeString(strings.Map(func(c rune) rune {
		switch c {
		case ' ', '\t', '\n':
			return -1 // ignore
		}
		return c
	}, s))
	if err != nil {
		panic(err)
	}
	return b
}

// testReader implements io.Reader.
type testReader struct {
	readFunc func([]byte) (int, error)
}

func (r testReader) Read(p []byte) (n int, err error) { return r.readFunc(p) }

// synctestSubtest runs f in a subtest in a synctest.Run bubble.
func synctestSubtest(t *testing.T, name string, f func(t *testing.T)) {
	t.Run(name, func(t *testing.T) {
		synctest.Test(t, f)
	})
}
