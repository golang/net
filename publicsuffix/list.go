// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package publicsuffix provides a public suffix list based on data from
// http://publicsuffix.org/. A public suffix is one under which Internet users
// can directly register names.
package publicsuffix

// TODO(nigeltao): do we need to distinguish between ICANN domains and private
// domains?

import (
	"exp/cookiejar"
	"strings"
)

// List implements cookiejar.PublicSuffixList using a copy of the
// publicsuffix.org database compiled into the library.
var List cookiejar.PublicSuffixList = list{}

type list struct{}

func (list) String() string {
	return version
}

func (list) PublicSuffix(domain string) string {
	lo, hi := uint32(0), uint32(numTLD)
	s, suffix, wildcard := domain, len(domain), false
loop:
	for {
		dot := strings.LastIndex(s, ".")
		if wildcard {
			suffix = 1 + dot
		}
		if lo == hi {
			break
		}
		f := find(s[1+dot:], lo, hi)
		if f == notFound {
			break
		}

		u := nodes[f][0]
		lo = u & 0xffff
		u >>= 16
		hi = u&0x1fff + lo
		u >>= 13
		wildcard = u&0x01 != 0
		u >>= 1
		switch u {
		case nodeTypeNormal:
			suffix = 1 + dot
		case nodeTypeException:
			suffix = 1 + len(s)
			break loop
		}

		if dot == -1 {
			break
		}
		s = s[:dot]
	}
	if suffix == len(domain) {
		// If no rules match, the prevailing rule is "*".
		return domain[1+strings.LastIndex(domain, "."):]
	}
	return domain[suffix:]
}

const notFound uint32 = 1<<32 - 1

// find returns the index of the node in the range [lo, hi) whose label equals
// label, or notFound if there is no such node. The range is assumed to be in
// strictly increasing node label order.
func find(label string, lo, hi uint32) uint32 {
	for lo < hi {
		mid := lo + (hi-lo)/2
		s := nodeLabel(mid)
		if s < label {
			lo = mid + 1
		} else if s == label {
			return mid
		} else {
			hi = mid
		}
	}
	return notFound
}

// nodeLabel returns the label for the i'th node.
func nodeLabel(i uint32) string {
	x := nodes[i][1]
	offset, length := x>>8, x&0xff
	return text[offset : offset+length]
}
