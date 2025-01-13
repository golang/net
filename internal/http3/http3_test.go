// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.24 && goexperiment.synctest

package http3

import (
	"os"
	"slices"
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

// runSynctest runs f in a synctest.Run bubble.
// It arranges for t.Cleanup functions to run within the bubble.
func runSynctest(t *testing.T, f func(t testing.TB)) {
	synctest.Run(func() {
		ct := &cleanupT{T: t}
		defer ct.done()
		f(ct)
	})
}

// runSynctestSubtest runs f in a subtest in a synctest.Run bubble.
func runSynctestSubtest(t *testing.T, name string, f func(t testing.TB)) {
	t.Run(name, func(t *testing.T) {
		runSynctest(t, f)
	})
}

// cleanupT wraps a testing.T and adds its own Cleanup method.
// Used to execute cleanup functions within a synctest bubble.
type cleanupT struct {
	*testing.T
	cleanups []func()
}

// Cleanup replaces T.Cleanup.
func (t *cleanupT) Cleanup(f func()) {
	t.cleanups = append(t.cleanups, f)
}

func (t *cleanupT) done() {
	for _, f := range slices.Backward(t.cleanups) {
		f()
	}
}
