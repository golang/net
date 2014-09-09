// Copyright 2014 The Go Authors.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

// Package hpack implements HPACK, a compression format for
// efficiently representing HTTP header fields in the context of HTTP/2.
//
// See http://tools.ietf.org/html/draft-ietf-httpbis-header-compression-09
package hpack

import "fmt"

// A HeaderField is a name-value pair. Both the name and value are
// treated as opaque sequences of octets.
type HeaderField struct {
	Name, Value string
}

func (hf *HeaderField) size() uint32 {
	// http://http2.github.io/http2-spec/compression.html#rfc.section.4.1
	// "The size of the dynamic table is the sum of the size of
	// its entries.  The size of an entry is the sum of its name's
	// length in octets (as defined in Section 5.2), its value's
	// length in octets (see Section 5.2), plus 32.  The size of
	// an entry is calculated using the length of the name and
	// value without any Huffman encoding applied."

	// This can overflow if somebody makes a large HeaderField
	// Name and/or Value by hand, but we don't care, because that
	// won't happen on the wire because the encoding doesn't allow
	// it.
	return uint32(len(hf.Name) + len(hf.Value) + 32)
}

// A Decoder is the decoding context for incremental processing of
// header blocks.
type Decoder struct {
	dynTab dynamicTable
	emit   func(f HeaderField, sensitive bool)
}

func NewDecoder(maxSize uint32, emitFunc func(f HeaderField, sensitive bool)) *Decoder {
	d := &Decoder{
		emit: emitFunc,
	}
	d.dynTab.setMaxSize(maxSize)
	return d
}

// TODO: add method *Decoder.Reset(maxSize, emitFunc) to let callers re-use Decoders and their
// underlying buffers for garbage reasons.

func (d *Decoder) SetMaxDynamicTableSize(v uint32) {
	d.dynTab.setMaxSize(v)
}

type dynamicTable struct {
	// s is the FIFO described at
	// http://http2.github.io/http2-spec/compression.html#rfc.section.2.3.2
	// The newest (low index) is append at the end, and items are
	// evicted from the front.
	s       []HeaderField
	size    uint32
	maxSize uint32
}

func (dt *dynamicTable) setMaxSize(v uint32) {
	// TODO: evictions
	dt.maxSize = v
}

// TODO: change dynamicTable to be a struct with a slice and a size int field,
// per http://http2.github.io/http2-spec/compression.html#rfc.section.4.1:
//
//
// Then make add increment the size. maybe the max size should move from Decoder to
// dynamicTable and add should return an ok bool if there was enough space.
//
// Later we'll need a remove operation on dynamicTable.

func (dt *dynamicTable) add(f HeaderField) {
	dt.s = append(dt.s, f)
	dt.size += f.size()
	for dt.size > dt.maxSize {
		// TODO: evict
		break
	}
}

func (dt *dynamicTable) at(i int) HeaderField {
	if i < 1 {
		panic(fmt.Sprintf("header table index %d too small", i))
	}
	max := len(dt.s) + len(staticTable)
	if i > max {
		panic(fmt.Sprintf("header table index %d too large (max = %d)", i, max))
	}
	if i <= len(staticTable) {
		return staticTable[i-1]
	}
	return dt.s[len(dt.s)-(i-len(staticTable))]
}
