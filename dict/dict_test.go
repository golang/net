// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dict

import "testing"

func TestUnquote(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{`hello\ world`, "hello world"},
		{`hello\\world`, `hello\world`},
		{`trailing\\`, `trailing\`},
		{`\\`, `\`},
		{`\`, ""},
		{`a\`, "a"},
		{`\a`, "a"},
	}
	for _, tt := range tests {
		got := unquote(tt.in)
		if got != tt.want {
			t.Errorf("unquote(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
