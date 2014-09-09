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
	dt     dynamicTable
	refSet struct{} // TODO
	Emit   func(f HeaderField, sensitive bool)
}

type dynamicTable []HeaderField

func (s *dynamicTable) add(f HeaderField) {
	*s = append(*s, f)
}

func (s *dynamicTable) at(i int) HeaderField {
	if i < 1 {
		panic(fmt.Sprintf("header table index %d too small", i))
	}
	dt := *s
	max := len(dt) + len(staticTable)
	if i > max {
		panic(fmt.Sprintf("header table index %d too large (max = %d)", i, max))
	}
	if i <= len(dt) {
		return dt[i-1]
	}
	return staticTable[i-len(dt)-1]
}
