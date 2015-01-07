// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"fmt"
	"math/rand"
	"path"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestWalkToRoot(t *testing.T) {
	testCases := []struct {
		name string
		want []string
	}{{
		"/a/b/c/d",
		[]string{
			"/a/b/c/d",
			"/a/b/c",
			"/a/b",
			"/a",
			"/",
		},
	}, {
		"/a",
		[]string{
			"/a",
			"/",
		},
	}, {
		"/",
		[]string{
			"/",
		},
	}}

	for _, tc := range testCases {
		var got []string
		if !walkToRoot(tc.name, func(name0 string, first bool) bool {
			if first != (len(got) == 0) {
				t.Errorf("name=%q: first=%t but len(got)==%d", tc.name, first, len(got))
				return false
			}
			got = append(got, name0)
			return true
		}) {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("name=%q:\ngot  %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// lockTestNames are the names of a set of mutually compatible locks. For each
// name fragment:
//	- _ means no explicit lock.
//	- i means a infinite-depth lock,
//	- z means a zero-depth lock,
var lockTestNames = []string{
	"/_/_/_/_/z",
	"/_/_/i",
	"/_/z",
	"/_/z/i",
	"/_/z/z",
	"/_/z/_/i",
	"/_/z/_/z",
	"/i",
	"/z",
	"/z/_/i",
	"/z/_/z",
}

func lockTestDepth(name string) int {
	switch name[len(name)-1] {
	case 'i':
		return -1
	case 'z':
		return 0
	}
	panic(fmt.Sprintf("lock name %q did not end with 'i' or 'z'", name))
}

func TestMemLSCanCreate(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMemLS().(*memLS)

	for _, name := range lockTestNames {
		_, err := m.Create(now, LockDetails{
			Depth:    lockTestDepth(name),
			Duration: -1,
			Root:     name,
		})
		if err != nil {
			t.Fatalf("creating lock for %q: %v", name, err)
		}
	}

	wantCanCreate := func(name string, depth int) bool {
		for _, n := range lockTestNames {
			switch {
			case n == name:
				// An existing lock has the same name as the proposed lock.
				return false
			case strings.HasPrefix(n, name):
				// An existing lock would be a child of the proposed lock,
				// which conflicts if the proposed lock has infinite depth.
				if depth < 0 {
					return false
				}
			case strings.HasPrefix(name, n):
				// An existing lock would be an ancestor of the proposed lock,
				// which conflicts if the ancestor has infinite depth.
				if n[len(n)-1] == 'i' {
					return false
				}
			}
		}
		return true
	}

	var check func(int, string)
	check = func(recursion int, name string) {
		for _, depth := range []int{-1, 0} {
			got := m.canCreate(name, depth)
			want := wantCanCreate(name, depth)
			if got != want {
				t.Errorf("canCreate name=%q depth=%d: got %t, want %t", name, depth, got, want)
			}
		}
		if recursion == 6 {
			return
		}
		if name != "/" {
			name += "/"
		}
		for _, c := range "_iz" {
			check(recursion+1, name+string(c))
		}
	}
	check(0, "/")
}

func TestMemLSCreateUnlock(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMemLS().(*memLS)
	rng := rand.New(rand.NewSource(0))
	tokens := map[string]string{}
	for i := 0; i < 1000; i++ {
		name := lockTestNames[rng.Intn(len(lockTestNames))]
		if token := tokens[name]; token != "" {
			if err := m.Unlock(now, token); err != nil {
				t.Fatalf("iteration #%d: unlock %q: %v", i, name, err)
			}
			tokens[name] = ""
		} else {
			token, err := m.Create(now, LockDetails{
				Depth:    lockTestDepth(name),
				Duration: -1,
				Root:     name,
			})
			if err != nil {
				t.Fatalf("iteration #%d: create %q: %v", i, name, err)
			}
			tokens[name] = token
		}
		if err := m.consistent(); err != nil {
			t.Fatalf("iteration #%d: inconsistent state: %v", i, err)
		}
	}
}

func (m *memLS) consistent() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// If m.byName is non-empty, then it must contain an entry for the root "/",
	// and its refCount should equal the number of locked nodes.
	if len(m.byName) > 0 {
		n := m.byName["/"]
		if n == nil {
			return fmt.Errorf(`non-empty m.byName does not contain the root "/"`)
		}
		if n.refCount != len(m.byToken) {
			return fmt.Errorf("root node refCount=%d, differs from len(m.byToken)=%d", n.refCount, len(m.byToken))
		}
	}

	for name, n := range m.byName {
		// The map keys should be consistent with the node's copy of the key.
		if n.details.Root != name {
			return fmt.Errorf("node name %q != byName map key %q", n.details.Root, name)
		}

		// A name must be clean, and start with a "/".
		if len(name) == 0 || name[0] != '/' {
			return fmt.Errorf(`node name %q does not start with "/"`, name)
		}
		if name != path.Clean(name) {
			return fmt.Errorf(`node name %q is not clean`, name)
		}

		// A node's refCount should be positive.
		if n.refCount <= 0 {
			return fmt.Errorf("non-positive refCount for node at name %q", name)
		}

		// A node's refCount should be the number of self-or-descendents that
		// are locked (i.e. have a non-empty token).
		var list []string
		for name0, n0 := range m.byName {
			// All of lockTestNames' name fragments are one byte long: '_', 'i' or 'z',
			// so strings.HasPrefix is equivalent to self-or-descendent name match.
			// We don't have to worry about "/foo/bar" being a false positive match
			// for "/foo/b".
			if strings.HasPrefix(name0, name) && n0.token != "" {
				list = append(list, name0)
			}
		}
		if n.refCount != len(list) {
			sort.Strings(list)
			return fmt.Errorf("node at name %q has refCount %d but locked self-or-descendents are %q (len=%d)",
				name, n.refCount, list, len(list))
		}

		// A node n is in m.byToken if it has a non-empty token.
		if n.token != "" {
			if _, ok := m.byToken[n.token]; !ok {
				return fmt.Errorf("node at name %q has token %q but not in m.byToken", name, n.token)
			}
		}
	}

	for token, n := range m.byToken {
		// The map keys should be consistent with the node's copy of the key.
		if n.token != token {
			return fmt.Errorf("node token %q != byToken map key %q", n.token, token)
		}

		// Every node in m.byToken is in m.byName.
		if _, ok := m.byName[n.details.Root]; !ok {
			return fmt.Errorf("node at name %q in m.byToken but not in m.byName", n.details.Root)
		}
	}
	return nil
}
