// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestSlashClean(t *testing.T) {
	testCases := []string{
		"",
		".",
		"/",
		"/./",
		"//",
		"//.",
		"//a",
		"/a",
		"/a/b/c",
		"/a//b/./../c/d/",
		"a",
		"a/b/c",
	}
	for _, tc := range testCases {
		got := slashClean(tc)
		want := path.Clean("/" + tc)
		if got != want {
			t.Errorf("tc=%q: got %q, want %q", tc, got, want)
		}
	}
}

func TestDirResolve(t *testing.T) {
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

		i, prevFrag := 0, ""
		err := fs.walk("test", tc.dir, func(dir *memFSNode, frag string, final bool) error {
			got := walkStep{
				name:  prevFrag,
				frag:  frag,
				final: final,
			}
			want := tc.want[i]

			if got != want {
				return fmt.Errorf("got %+v, want %+v", got, want)
			}
			i, prevFrag = i+1, frag
			return nil
		})
		if err != nil {
			t.Errorf("tc.dir=%q: %v", tc.dir, err)
		}
	}
}

// find appends to ss the names of the named file and its children. It is
// analogous to the Unix find command.
//
// The returned strings are not guaranteed to be in any particular order.
func find(ss []string, fs FileSystem, name string) ([]string, error) {
	stat, err := fs.Stat(name)
	if err != nil {
		return nil, err
	}
	ss = append(ss, name)
	if stat.IsDir() {
		f, err := fs.OpenFile(name, os.O_RDONLY, 0)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		children, err := f.Readdir(-1)
		if err != nil {
			return nil, err
		}
		for _, c := range children {
			ss, err = find(ss, fs, path.Join(name, c.Name()))
			if err != nil {
				return nil, err
			}
		}
	}
	return ss, nil
}

func testFS(t *testing.T, fs FileSystem) {
	errStr := func(err error) string {
		switch {
		case os.IsExist(err):
			return "errExist"
		case os.IsNotExist(err):
			return "errNotExist"
		case err != nil:
			return "err"
		}
		return "ok"
	}

	// The non-"find" non-"stat" test cases should change the file system state. The
	// indentation of the "find"s and "stat"s helps distinguish such test cases.
	testCases := []string{
		"  stat / want dir",
		"  stat /a want errNotExist",
		"  stat /d want errNotExist",
		"  stat /d/e want errNotExist",
		"create /a A want ok",
		"  stat /a want 1",
		"create /d/e EEE want errNotExist",
		"mk-dir /a want errExist",
		"mk-dir /d/m want errNotExist",
		"mk-dir /d want ok",
		"  stat /d want dir",
		"create /d/e EEE want ok",
		"  stat /d/e want 3",
		"  find / /a /d /d/e",
		"create /d/f FFFF want ok",
		"create /d/g GGGGGGG want ok",
		"mk-dir /d/m want ok",
		"mk-dir /d/m want errExist",
		"create /d/m/p PPPPP want ok",
		"  stat /d/e want 3",
		"  stat /d/f want 4",
		"  stat /d/g want 7",
		"  stat /d/h want errNotExist",
		"  stat /d/m want dir",
		"  stat /d/m/p want 5",
		"  find / /a /d /d/e /d/f /d/g /d/m /d/m/p",
		"rm-all /d want ok",
		"  stat /a want 1",
		"  stat /d want errNotExist",
		"  stat /d/e want errNotExist",
		"  stat /d/f want errNotExist",
		"  stat /d/g want errNotExist",
		"  stat /d/m want errNotExist",
		"  stat /d/m/p want errNotExist",
		"  find / /a",
		"mk-dir /d/m want errNotExist",
		"mk-dir /d want ok",
		"create /d/f FFFF want ok",
		"rm-all /d/f want ok",
		"mk-dir /d/m want ok",
		"rm-all /z want ok",
		"rm-all / want err",
		"create /b BB want ok",
		"  stat / want dir",
		"  stat /a want 1",
		"  stat /b want 2",
		"  stat /c want errNotExist",
		"  stat /d want dir",
		"  stat /d/m want dir",
		"  find / /a /b /d /d/m",
		"rename /b /b want ok",
		"  stat /b want 2",
		"  stat /c want errNotExist",
		"rename /b /c want ok",
		"  stat /b want errNotExist",
		"  stat /c want 2",
		"  stat /d/m want dir",
		"  stat /d/n want errNotExist",
		"  find / /a /c /d /d/m",
		"rename /d/m /d/n want ok",
		"create /d/n/q QQQQ want ok",
		"  stat /d/m want errNotExist",
		"  stat /d/n want dir",
		"  stat /d/n/q want 4",
		"rename /d /d/n/z want err",
		"rename /c /d/n/q want ok",
		"  stat /c want errNotExist",
		"  stat /d/n/q want 2",
		"  find / /a /d /d/n /d/n/q",
		"create /d/n/r RRRRR want ok",
		"mk-dir /u want ok",
		"mk-dir /u/v want ok",
		"rename /d/n /u want err",
		"create /t TTTTTT want ok",
		"rename /d/n /t want err",
		"rm-all /t want ok",
		"rename /d/n /t want ok",
		"  stat /d want dir",
		"  stat /d/n want errNotExist",
		"  stat /d/n/r want errNotExist",
		"  stat /t want dir",
		"  stat /t/q want 2",
		"  stat /t/r want 5",
		"  find / /a /d /t /t/q /t/r /u /u/v",
		"rename /t / want err",
		"rename /t /u/v want ok",
		"  stat /u/v/r want 5",
		"rename / /z want err",
		"  find / /a /d /u /u/v /u/v/q /u/v/r",
		"  stat /a want 1",
		"  stat /b want errNotExist",
		"  stat /c want errNotExist",
		"  stat /u/v/r want 5",
		"copy__ o=F d=0 /a /b want ok",
		"copy__ o=T d=0 /a /c want ok",
		"  stat /a want 1",
		"  stat /b want 1",
		"  stat /c want 1",
		"  stat /u/v/r want 5",
		"copy__ o=F d=0 /u/v/r /b want errExist",
		"  stat /b want 1",
		"copy__ o=T d=0 /u/v/r /b want ok",
		"  stat /a want 1",
		"  stat /b want 5",
		"  stat /u/v/r want 5",
		"rm-all /a want ok",
		"rm-all /b want ok",
		"mk-dir /u/v/w want ok",
		"create /u/v/w/s SSSSSSSS want ok",
		"  stat /d want dir",
		"  stat /d/x want errNotExist",
		"  stat /d/y want errNotExist",
		"  stat /u/v/r want 5",
		"  stat /u/v/w/s want 8",
		"  find / /c /d /u /u/v /u/v/q /u/v/r /u/v/w /u/v/w/s",
		"copy__ o=T d=0 /u/v /d/x want ok",
		"copy__ o=T d=∞ /u/v /d/y want ok",
		"rm-all /u want ok",
		"  stat /d/x want dir",
		"  stat /d/x/q want errNotExist",
		"  stat /d/x/r want errNotExist",
		"  stat /d/x/w want errNotExist",
		"  stat /d/x/w/s want errNotExist",
		"  stat /d/y want dir",
		"  stat /d/y/q want 2",
		"  stat /d/y/r want 5",
		"  stat /d/y/w want dir",
		"  stat /d/y/w/s want 8",
		"  stat /u want errNotExist",
		"  find / /c /d /d/x /d/y /d/y/q /d/y/r /d/y/w /d/y/w/s",
		"copy__ o=F d=∞ /d/y /d/x want errExist",
	}

	for i, tc := range testCases {
		tc = strings.TrimSpace(tc)
		j := strings.IndexByte(tc, ' ')
		if j < 0 {
			t.Fatalf("test case #%d %q: invalid command", i, tc)
		}
		op, arg := tc[:j], tc[j+1:]

		switch op {
		default:
			t.Fatalf("test case #%d %q: invalid operation %q", i, tc, op)

		case "create":
			parts := strings.Split(arg, " ")
			if len(parts) != 4 || parts[2] != "want" {
				t.Fatalf("test case #%d %q: invalid write", i, tc)
			}
			f, opErr := fs.OpenFile(parts[0], os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
			if got := errStr(opErr); got != parts[3] {
				t.Fatalf("test case #%d %q: OpenFile: got %q (%v), want %q", i, tc, got, opErr, parts[3])
			}
			if f != nil {
				if _, err := f.Write([]byte(parts[1])); err != nil {
					t.Fatalf("test case #%d %q: Write: %v", i, tc, err)
				}
				if err := f.Close(); err != nil {
					t.Fatalf("test case #%d %q: Close: %v", i, tc, err)
				}
			}

		case "find":
			got, err := find(nil, fs, "/")
			if err != nil {
				t.Fatalf("test case #%d %q: find: %v", i, tc, err)
			}
			sort.Strings(got)
			want := strings.Split(arg, " ")
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("test case #%d %q:\ngot  %s\nwant %s", i, tc, got, want)
			}

		case "copy__", "mk-dir", "rename", "rm-all", "stat":
			nParts := 3
			switch op {
			case "copy__":
				nParts = 6
			case "rename":
				nParts = 4
			}
			parts := strings.Split(arg, " ")
			if len(parts) != nParts {
				t.Fatalf("test case #%d %q: invalid %s", i, tc, op)
			}

			got, opErr := "", error(nil)
			switch op {
			case "copy__":
				overwrite, depth := false, 0
				if parts[0] == "o=T" {
					overwrite = true
				}
				if parts[1] == "d=∞" {
					depth = infiniteDepth
				}
				_, opErr = copyFiles(fs, parts[2], parts[3], overwrite, depth, 0)
			case "mk-dir":
				opErr = fs.Mkdir(parts[0], 0777)
			case "rename":
				opErr = fs.Rename(parts[0], parts[1])
			case "rm-all":
				opErr = fs.RemoveAll(parts[0])
			case "stat":
				var stat os.FileInfo
				fileName := parts[0]
				if stat, opErr = fs.Stat(fileName); opErr == nil {
					if stat.IsDir() {
						got = "dir"
					} else {
						got = strconv.Itoa(int(stat.Size()))
					}

					if fileName == "/" {
						// For a Dir FileSystem, the virtual file system root maps to a
						// real file system name like "/tmp/webdav-test012345", which does
						// not end with "/". We skip such cases.
					} else if statName := stat.Name(); path.Base(fileName) != statName {
						t.Fatalf("test case #%d %q: file name %q inconsistent with stat name %q",
							i, tc, fileName, statName)
					}
				}
			}
			if got == "" {
				got = errStr(opErr)
			}

			if parts[len(parts)-2] != "want" {
				t.Fatalf("test case #%d %q: invalid %s", i, tc, op)
			}
			if want := parts[len(parts)-1]; got != want {
				t.Fatalf("test case #%d %q: got %q (%v), want %q", i, tc, got, opErr, want)
			}
		}
	}
}

func TestDir(t *testing.T) {
	td, err := ioutil.TempDir("", "webdav-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(td)
	testFS(t, Dir(td))
}

func TestMemFS(t *testing.T) {
	testFS(t, NewMemFS())
}

func TestMemFSRoot(t *testing.T) {
	fs := NewMemFS()
	for i := 0; i < 5; i++ {
		stat, err := fs.Stat("/")
		if err != nil {
			t.Fatalf("i=%d: Stat: %v", i, err)
		}
		if !stat.IsDir() {
			t.Fatalf("i=%d: Stat.IsDir is false, want true", i)
		}

		f, err := fs.OpenFile("/", os.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("i=%d: OpenFile: %v", i, err)
		}
		defer f.Close()
		children, err := f.Readdir(-1)
		if err != nil {
			t.Fatalf("i=%d: Readdir: %v", i, err)
		}
		if len(children) != i {
			t.Fatalf("i=%d: got %d children, want %d", i, len(children), i)
		}

		if _, err := f.Write(make([]byte, 1)); err == nil {
			t.Fatalf("i=%d: Write: got nil error, want non-nil", i)
		}

		if err := fs.Mkdir(fmt.Sprintf("/dir%d", i), 0777); err != nil {
			t.Fatalf("i=%d: Mkdir: %v", i, err)
		}
	}
}

func TestMemFileReaddir(t *testing.T) {
	fs := NewMemFS()
	if err := fs.Mkdir("/foo", 0777); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	readdir := func(count int) ([]os.FileInfo, error) {
		f, err := fs.OpenFile("/foo", os.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		return f.Readdir(count)
	}
	if got, err := readdir(-1); len(got) != 0 || err != nil {
		t.Fatalf("readdir(-1): got %d fileInfos with err=%v, want 0, <nil>", len(got), err)
	}
	if got, err := readdir(+1); len(got) != 0 || err != io.EOF {
		t.Fatalf("readdir(+1): got %d fileInfos with err=%v, want 0, EOF", len(got), err)
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
