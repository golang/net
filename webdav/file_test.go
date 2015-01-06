// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestDir(t *testing.T) {
	testCases := []struct {
		dir, name, want string
	}{
		{"/", "", "/"},
		{"/", "/", "/"},
		{"/", ".", "/"},
		{"/", "./a", "/a"},
		{"/", "..", "/"},
		{"/", "..", "/"},
		{"/", "../", "/"},
		{"/", "../.", "/"},
		{"/", "../a", "/a"},
		{"/", "../..", "/"},
		{"/", "../bar/a", "/bar/a"},
		{"/", "../baz/a", "/baz/a"},
		{"/", "...", "/..."},
		{"/", ".../a", "/.../a"},
		{"/", ".../..", "/"},
		{"/", "a", "/a"},
		{"/", "a/./b", "/a/b"},
		{"/", "a/../../b", "/b"},
		{"/", "a/../b", "/b"},
		{"/", "a/b", "/a/b"},
		{"/", "a/b/c/../../d", "/a/d"},
		{"/", "a/b/c/../../../d", "/d"},
		{"/", "a/b/c/../../../../d", "/d"},
		{"/", "a/b/c/d", "/a/b/c/d"},

		{"/foo/bar", "", "/foo/bar"},
		{"/foo/bar", "/", "/foo/bar"},
		{"/foo/bar", ".", "/foo/bar"},
		{"/foo/bar", "./a", "/foo/bar/a"},
		{"/foo/bar", "..", "/foo/bar"},
		{"/foo/bar", "../", "/foo/bar"},
		{"/foo/bar", "../.", "/foo/bar"},
		{"/foo/bar", "../a", "/foo/bar/a"},
		{"/foo/bar", "../..", "/foo/bar"},
		{"/foo/bar", "../bar/a", "/foo/bar/bar/a"},
		{"/foo/bar", "../baz/a", "/foo/bar/baz/a"},
		{"/foo/bar", "...", "/foo/bar/..."},
		{"/foo/bar", ".../a", "/foo/bar/.../a"},
		{"/foo/bar", ".../..", "/foo/bar"},
		{"/foo/bar", "a", "/foo/bar/a"},
		{"/foo/bar", "a/./b", "/foo/bar/a/b"},
		{"/foo/bar", "a/../../b", "/foo/bar/b"},
		{"/foo/bar", "a/../b", "/foo/bar/b"},
		{"/foo/bar", "a/b", "/foo/bar/a/b"},
		{"/foo/bar", "a/b/c/../../d", "/foo/bar/a/d"},
		{"/foo/bar", "a/b/c/../../../d", "/foo/bar/d"},
		{"/foo/bar", "a/b/c/../../../../d", "/foo/bar/d"},
		{"/foo/bar", "a/b/c/d", "/foo/bar/a/b/c/d"},

		{"/foo/bar/", "", "/foo/bar"},
		{"/foo/bar/", "/", "/foo/bar"},
		{"/foo/bar/", ".", "/foo/bar"},
		{"/foo/bar/", "./a", "/foo/bar/a"},
		{"/foo/bar/", "..", "/foo/bar"},

		{"/foo//bar///", "", "/foo/bar"},
		{"/foo//bar///", "/", "/foo/bar"},
		{"/foo//bar///", ".", "/foo/bar"},
		{"/foo//bar///", "./a", "/foo/bar/a"},
		{"/foo//bar///", "..", "/foo/bar"},

		{"/x/y/z", "ab/c\x00d/ef", ""},

		{".", "", "."},
		{".", "/", "."},
		{".", ".", "."},
		{".", "./a", "a"},
		{".", "..", "."},
		{".", "..", "."},
		{".", "../", "."},
		{".", "../.", "."},
		{".", "../a", "a"},
		{".", "../..", "."},
		{".", "../bar/a", "bar/a"},
		{".", "../baz/a", "baz/a"},
		{".", "...", "..."},
		{".", ".../a", ".../a"},
		{".", ".../..", "."},
		{".", "a", "a"},
		{".", "a/./b", "a/b"},
		{".", "a/../../b", "b"},
		{".", "a/../b", "b"},
		{".", "a/b", "a/b"},
		{".", "a/b/c/../../d", "a/d"},
		{".", "a/b/c/../../../d", "d"},
		{".", "a/b/c/../../../../d", "d"},
		{".", "a/b/c/d", "a/b/c/d"},

		{"", "", "."},
		{"", "/", "."},
		{"", ".", "."},
		{"", "./a", "a"},
		{"", "..", "."},
	}

	for _, tc := range testCases {
		d := Dir(filepath.FromSlash(tc.dir))
		if got := filepath.ToSlash(d.resolve(tc.name)); got != tc.want {
			t.Errorf("dir=%q, name=%q: got %q, want %q", tc.dir, tc.name, got, tc.want)
		}
	}
}

func TestWalk(t *testing.T) {
	type walkStep struct {
		name, frag string
		final      bool
	}

	testCases := []struct {
		dir  string
		want []walkStep
	}{
		{"", []walkStep{
			{"", "", true},
		}},
		{"/", []walkStep{
			{"", "", true},
		}},
		{"/a", []walkStep{
			{"", "a", true},
		}},
		{"/a/", []walkStep{
			{"", "a", true},
		}},
		{"/a/b", []walkStep{
			{"", "a", false},
			{"a", "b", true},
		}},
		{"/a/b/", []walkStep{
			{"", "a", false},
			{"a", "b", true},
		}},
		{"/a/b/c", []walkStep{
			{"", "a", false},
			{"a", "b", false},
			{"b", "c", true},
		}},
		// The following test case is the one mentioned explicitly
		// in the method description.
		{"/foo/bar/x", []walkStep{
			{"", "foo", false},
			{"foo", "bar", false},
			{"bar", "x", true},
		}},
	}

	for _, tc := range testCases {
		fs := NewMemFS().(*memFS)

		parts := strings.Split(tc.dir, "/")
		for p := 2; p < len(parts); p++ {
			d := strings.Join(parts[:p], "/")
			if err := fs.Mkdir(d, 0666); err != nil {
				t.Errorf("tc.dir=%q: mkdir: %q: %v", tc.dir, d, err)
			}
		}

		i := 0
		err := fs.walk("test", tc.dir, func(dir *memFSNode, frag string, final bool) error {
			got := walkStep{
				name:  dir.name,
				frag:  frag,
				final: final,
			}
			want := tc.want[i]

			if got != want {
				return fmt.Errorf("got %+v, want %+v", got, want)
			}
			i++
			return nil
		})
		if err != nil {
			t.Errorf("tc.dir=%q: %v", tc.dir, err)
		}
	}
}
