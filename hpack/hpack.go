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

	// TODO: max table size http://http2.github.io/http2-spec/compression.html#maximum.table.size
	//
}

type dynamicTable []HeaderField

// TODO: change dynamicTable to be a struct with a slice and a size int field,
// per http://http2.github.io/http2-spec/compression.html#rfc.section.4.1:
//
// "The size of the dynamic table is the sum of the size of its
// entries.  The size of an entry is the sum of its name's length in
// octets (as defined in Section 5.2), its value's length in octets
// (see Section 5.2), plus 32.  The size of an entry is calculated
// using the length of the name and value without any Huffman encoding
// applied.  "
//
// Then make add increment the size. maybe the max size should move from Decoder to
// dynamicTable and add should return an ok bool if there was enough space.
//
// Later we'll need a remove operation on dynamicTable.

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

// TODO: new methods on dynamicTable for changing table max table size & eviction:
// http://http2.github.io/http2-spec/compression.html#rfc.section.4.2
