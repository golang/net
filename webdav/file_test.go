// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
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

func TestMemFile(t *testing.T) {
	testCases := []string{
		"wantData ",
		"wantSize 0",
		"write abc",
		"wantData abc",
		"write de",
		"wantData abcde",
		"wantSize 5",
		"write 5*x",
		"write 4*y+2*z",
		"write 3*st",
		"wantData abcdexxxxxyyyyzzststst",
		"wantSize 22",
		"seek set 4 want 4",
		"write EFG",
		"wantData abcdEFGxxxyyyyzzststst",
		"wantSize 22",
		"seek set 2 want 2",
		"read cdEF",
		"read Gx",
		"seek cur 0 want 8",
		"seek cur 2 want 10",
		"seek cur -1 want 9",
		"write J",
		"wantData abcdEFGxxJyyyyzzststst",
		"wantSize 22",
		"seek cur -4 want 6",
		"write ghijk",
		"wantData abcdEFghijkyyyzzststst",
		"wantSize 22",
		"read yyyz",
		"seek cur 0 want 15",
		"write ",
		"seek cur 0 want 15",
		"read ",
		"seek cur 0 want 15",
		"seek end -3 want 19",
		"write ZZ",
		"wantData abcdEFghijkyyyzzstsZZt",
		"wantSize 22",
		"write 4*A",
		"wantData abcdEFghijkyyyzzstsZZAAAA",
		"wantSize 25",
		"seek end 0 want 25",
		"seek end -5 want 20",
		"read Z+4*A",
		"write 5*B",
		"wantData abcdEFghijkyyyzzstsZZAAAABBBBB",
		"wantSize 30",
		"seek end 10 want 40",
		"write C",
		"wantData abcdEFghijkyyyzzstsZZAAAABBBBB..........C",
		"wantSize 41",
		"write D",
		"wantData abcdEFghijkyyyzzstsZZAAAABBBBB..........CD",
		"wantSize 42",
		"seek set 43 want 43",
		"write E",
		"wantData abcdEFghijkyyyzzstsZZAAAABBBBB..........CD.E",
		"wantSize 44",
		"seek set 0 want 0",
		"write 5*123456789_",
		"wantData 123456789_123456789_123456789_123456789_123456789_",
		"wantSize 50",
		"seek cur 0 want 50",
		"seek cur -99 want err",
	}

	const filename = "/foo"
	fs := NewMemFS()
	f, err := fs.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	for i, tc := range testCases {
		j := strings.IndexByte(tc, ' ')
		if j < 0 {
			t.Fatalf("test case #%d %q: invalid command", i, tc)
		}
		op, arg := tc[:j], tc[j+1:]

		// Expand an arg like "3*a+2*b" to "aaabb".
		parts := strings.Split(arg, "+")
		for j, part := range parts {
			if k := strings.IndexByte(part, '*'); k >= 0 {
				repeatCount, repeatStr := part[:k], part[k+1:]
				n, err := strconv.Atoi(repeatCount)
				if err != nil {
					t.Fatalf("test case #%d %q: invalid repeat count %q", i, tc, repeatCount)
				}
				parts[j] = strings.Repeat(repeatStr, n)
			}
		}
		arg = strings.Join(parts, "")

		switch op {
		default:
			t.Fatalf("test case #%d %q: invalid operation %q", i, tc, op)

		case "read":
			buf := make([]byte, len(arg))
			if _, err := io.ReadFull(f, buf); err != nil {
				t.Fatalf("test case #%d %q: ReadFull: %v", i, tc, err)
			}
			if got := string(buf); got != arg {
				t.Fatalf("test case #%d %q:\ngot  %q\nwant %q", i, tc, got, arg)
			}

		case "seek":
			parts := strings.Split(arg, " ")
			if len(parts) != 4 {
				t.Fatalf("test case #%d %q: invalid seek", i, tc)
			}

			whence := 0
			switch parts[0] {
			default:
				t.Fatalf("test case #%d %q: invalid seek whence", i, tc)
			case "set":
				whence = os.SEEK_SET
			case "cur":
				whence = os.SEEK_CUR
			case "end":
				whence = os.SEEK_END
			}
			offset, err := strconv.Atoi(parts[1])
			if err != nil {
				t.Fatalf("test case #%d %q: invalid offset %q", i, tc, parts[1])
			}

			if parts[2] != "want" {
				t.Fatalf("test case #%d %q: invalid seek", i, tc)
			}
			if parts[3] == "err" {
				_, err := f.Seek(int64(offset), whence)
				if err == nil {
					t.Fatalf("test case #%d %q: Seek returned nil error, want non-nil", i, tc)
				}
			} else {
				got, err := f.Seek(int64(offset), whence)
				if err != nil {
					t.Fatalf("test case #%d %q: Seek: %v", i, tc, err)
				}
				want, err := strconv.Atoi(parts[3])
				if err != nil {
					t.Fatalf("test case #%d %q: invalid want %q", i, tc, parts[3])
				}
				if got != int64(want) {
					t.Fatalf("test case #%d %q: got %d, want %d", i, tc, got, want)
				}
			}

		case "write":
			n, err := f.Write([]byte(arg))
			if err != nil {
				t.Fatalf("test case #%d %q: write: %v", i, tc, err)
			}
			if n != len(arg) {
				t.Fatalf("test case #%d %q: write returned %d bytes, want %d", i, tc, n, len(arg))
			}

		case "wantData":
			g, err := fs.OpenFile(filename, os.O_RDONLY, 0666)
			if err != nil {
				t.Fatalf("test case #%d %q: OpenFile: %v", i, tc, err)
			}
			gotBytes, err := ioutil.ReadAll(g)
			if err != nil {
				t.Fatalf("test case #%d %q: ReadAll: %v", i, tc, err)
			}
			for i, c := range gotBytes {
				if c == '\x00' {
					gotBytes[i] = '.'
				}
			}
			got := string(gotBytes)
			if got != arg {
				t.Fatalf("test case #%d %q:\ngot  %q\nwant %q", i, tc, got, arg)
			}
			if err := g.Close(); err != nil {
				t.Fatalf("test case #%d %q: Close: %v", i, tc, err)
			}

		case "wantSize":
			n, err := strconv.Atoi(arg)
			if err != nil {
				t.Fatalf("test case #%d %q: invalid size %q", i, tc, arg)
			}
			fi, err := fs.Stat(filename)
			if err != nil {
				t.Fatalf("test case #%d %q: Stat: %v", i, tc, err)
			}
			if got, want := fi.Size(), int64(n); got != want {
				t.Fatalf("test case #%d %q: got %d, want %d", i, tc, got, want)
			}
		}
	}
}

// TestMemFileWriteAllocs tests that writing N consecutive 1KiB chunks to a
// memFile doesn't allocate a new buffer for each of those N times. Otherwise,
// calling io.Copy(aMemFile, src) is likely to have quadratic complexity.
func TestMemFileWriteAllocs(t *testing.T) {
	fs := NewMemFS()
	f, err := fs.OpenFile("/xxx", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	xxx := make([]byte, 1024)
	for i := range xxx {
		xxx[i] = 'x'
	}

	a := testing.AllocsPerRun(100, func() {
		f.Write(xxx)
	})
	// AllocsPerRun returns an integral value, so we compare the rounded-down
	// number to zero.
	if a > 0 {
		t.Fatalf("%v allocs per run, want 0", a)
	}
}

func BenchmarkMemFileWrite(b *testing.B) {
	fs := NewMemFS()
	xxx := make([]byte, 1024)
	for i := range xxx {
		xxx[i] = 'x'
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, err := fs.OpenFile("/xxx", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
		if err != nil {
			b.Fatalf("OpenFile: %v", err)
		}
		for j := 0; j < 100; j++ {
			f.Write(xxx)
		}
		if err := f.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
		if err := fs.RemoveAll("/xxx"); err != nil {
			b.Fatalf("RemoveAll: %v", err)
		}
	}
}
