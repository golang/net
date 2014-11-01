// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build darwin dragonfly freebsd netbsd openbsd

package ipv6

func (f *sysICMPv6Filter) set(typ ICMPType, block bool) {
	if block {
		f.Filt[typ>>5] &^= 1 << (uint32(typ) & 31)
	} else {
		f.Filt[typ>>5] |= 1 << (uint32(typ) & 31)
	}
}

func (f *sysICMPv6Filter) setAll(block bool) {
	for i := range f.Filt {
		if block {
			f.Filt[i] = 0
		} else {
			f.Filt[i] = 1<<32 - 1
		}
	}
}

func (f *sysICMPv6Filter) willBlock(typ ICMPType) bool {
	return f.Filt[typ>>5]&(1<<(uint32(typ)&31)) == 0
}
