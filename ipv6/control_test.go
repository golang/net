// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipv6

import (
	"sync"
	"testing"
)

func TestControlFlags(t *testing.T) {
	tf := FlagInterface | FlagPathMTU
	opt := rawOpt{cflags: tf | FlagHopLimit}

	type ffn func(ControlFlags)
	var wg sync.WaitGroup
	for _, fn := range []ffn{opt.set, opt.clear, opt.clear} {
		wg.Add(1)
		go func(fn ffn) {
			defer wg.Done()
			opt.Lock()
			defer opt.Unlock()
			fn(tf)
		}(fn)
	}
	wg.Wait()

	if opt.isset(tf) {
		t.Fatalf("got %#x; expected %#x", opt.cflags, FlagHopLimit)
	}
}
