// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.rangefunc

package html

import (
	"strings"
	"testing"
)

func TestNode_ChildNodes(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"<a></a>", ""},
		{"<a><b></b></a>", "b"},
		{"<a>b</a>", "b"},
		{"<a><!--b--></a>", "b"},
		{"<a>b<c></c>d</a>", "b c d"},
		{"<a>b<c><!--d--></c>e</a>", "b c e"},
		{"<a><b><c>d<!--e-->f</c></b>g<!--h--><i>j</i></a>", "b g h i"},
	}
	for _, test := range tests {
		doc, err := Parse(strings.NewReader(test.in))
		if err != nil {
			t.Fatal(err)
		}
		// Drill to <html><head></head><body> test.in
		n := doc.FirstChild.FirstChild.NextSibling.FirstChild
		var results []string
		for c := range n.ChildNodes() {
			results = append(results, c.Data)
		}
		if got := strings.Join(results, " "); got != test.want {
			t.Errorf("unexpected children yielded by ChildNodes; want: %q got: %q", test.want, got)
		}
	}
}

func TestNode_All(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"<a></a>", "a"},
		{"<a><b></b></a>", "a b"},
		{"<a>b</a>", "a b"},
		{"<a><!--b--></a>", "a b"},
		{"<a>b<c></c>d</a>", "a b c d"},
		{"<a>b<c><!--d--></c>e</a>", "a b c d e"},
		{"<a><b><c>d<!--e-->f</c></b>g<!--h--><i>j</i></a>", "a b c d e f g h i j"},
	}
	for _, test := range tests {
		doc, err := Parse(strings.NewReader(test.in))
		if err != nil {
			t.Fatal(err)
		}
		// Drill to <html><head></head><body> test.in
		n := doc.FirstChild.FirstChild.NextSibling.FirstChild
		var results []string
		for c := range n.All() {
			results = append(results, c.Data)
		}
		if got := strings.Join(results, " "); got != test.want {
			t.Errorf("unexpected children yielded by All; want: %q got: %q",
				test.want, got)
		}
	}
}
