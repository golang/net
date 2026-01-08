// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.25

package http3

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestFiles checks that every test file in this package has a build constraint
// on Go 1.25. Tests rely on synctest.Test, added in Go 1.25.
//
// TODO(nsh): drop this test and Go 1.25 build contraints once x/net go.mod
// depends on 1.25 or newer.
func TestFiles(t *testing.T) {
	f, err := os.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	names, err := f.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		// Check for copyright header while we're in here.
		if !bytes.Contains(b, []byte("The Go Authors.")) {
			t.Errorf("%v: missing copyright", name)
		}
		// doc.go doesn't need a build constraint.
		if name == "doc.go" {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			if !bytes.Contains(b, []byte("//go:build go1.25")) {
				t.Errorf("%v: missing constraint on go1.25", name)
			}
		}
	}
}
