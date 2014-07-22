// Copyright 2014 The Go Authors.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

// Package hpack implements HPACK, a compression format for
// efficiently representing HTTP header fields in the context of HTTP/2.
//
// See http://tools.ietf.org/html/draft-ietf-httpbis-header-compression-08
package hpack

import "fmt"

// A HeaderField is a name-value pair. Both the name and value are
// treated as opaque sequences of octets.
type HeaderField struct {
	Name, Value string
}

// A Decoder is the decoding context for incremental processing of
// header blocks.
type Decoder struct {
	ht     headerTable
	refSet struct{} // TODO
	Emit   func(f HeaderField, sensitive bool)
}

type headerTable []HeaderField

func (s *headerTable) add(f HeaderField) {
	*s = append(*s, f)
}

func (s *headerTable) at(i int) HeaderField {
	if i < 1 {
		panic(fmt.Sprintf("header table index %d too small", i))
	}
	ht := *s
	max := len(ht) + len(staticTable)
	if i > max {
		panic(fmt.Sprintf("header table index %d too large (max = %d)", i, max))
	}
	if i <= len(ht) {
		return ht[i-1]
	}
	return staticTable[i-len(ht)-1]
}
