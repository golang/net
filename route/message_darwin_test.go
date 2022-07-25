// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package route

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestFetchAndParseRIBOnDarwin(t *testing.T) {
	for _, typ := range []RIBType{unix.NET_RT_FLAGS, unix.NET_RT_DUMP2, unix.NET_RT_IFLIST2} {
		var lastErr error
		var ms []Message
		for _, af := range []int{unix.AF_UNSPEC, unix.AF_INET, unix.AF_INET6} {
			rs, err := fetchAndParseRIB(af, typ)
			if err != nil {
				lastErr = err
				continue
			}
			ms = append(ms, rs...)
		}
		if len(ms) == 0 && lastErr != nil {
			t.Error(typ, lastErr)
			continue
		}
		ss, err := msgs(ms).validate()
		if err != nil {
			t.Error(typ, err)
			continue
		}
		for _, s := range ss {
			t.Log(s)
		}
	}
}
