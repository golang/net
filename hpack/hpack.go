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

// A DecodingError is something the spec defines as a decoding error.
type DecodingError struct {
	Err error
}

func (de DecodingError) Error() string {
	return fmt.Sprintf("decoding error: %v", de.Err)
}

// An InvalidIndexError is returned when an encoder references a table
// entry before the static table or after the end of the dynamic table.
type InvalidIndexError int

func (e InvalidIndexError) Error() string {
	return fmt.Sprintf("invalid indexed representation index %d", int(e))
}

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
	ents    []HeaderField
	size    uint32
	maxSize uint32
}

func (dt *dynamicTable) setMaxSize(v uint32) {
	dt.maxSize = v
	dt.evict()
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
	dt.ents = append(dt.ents, f)
	dt.size += f.size()
	dt.evict()
}

// If we're too big, evict old stuff (front of the slice)
func (dt *dynamicTable) evict() {
	base := dt.ents // keep base pointer of slice
	for dt.size > dt.maxSize {
		dt.size -= dt.ents[0].size()
		dt.ents = dt.ents[1:]
	}

	// Shift slice contents down if we evicted things.
	if len(dt.ents) != len(base) {
		copy(base, dt.ents)
		dt.ents = base[:len(dt.ents)]
	}
}

func (d *Decoder) at(i int) (hf HeaderField, ok bool) {
	if i < 1 {
		return
	}
	dents := d.dynTab.ents
	max := len(dents) + len(staticTable)
	if i > max {
		return
	}
	if i <= len(staticTable) {
		return staticTable[i-1], true
	}
	return dents[len(dents)-(i-len(staticTable))], true
}

// Decode decodes an entire block.
//
// TODO: remove this method and make it incremental later? This is
// easier for debugging now.
func (d *Decoder) Decode(p []byte) ([]HeaderField, error) {
	var hf []HeaderField
	// TODO: This is trashy. temporary development aid.
	saveFunc := d.emit
	defer func() { d.emit = saveFunc }()
	d.emit = func(f HeaderField, sensitive bool) {
		hf = append(hf, f)
	}

	for len(p) > 0 {
		// Look at first byte to see what we're dealing with.
		switch {
		case p[0]&(1<<7) != 0:
			// Indexed representation.
			// http://http2.github.io/http2-spec/compression.html#rfc.section.6.1
			idx := p[0] & ((1 << 7) - 1)
			if idx == 127 {
				panic("TODO: varuint decoding")
			}
			hf, ok := d.at(int(idx))
			if !ok {
				return nil, DecodingError{InvalidIndexError(idx)}
			}
			d.emit(hf, false /* TODO: sensitive ? */)
			p = p[1:]
		default:
			panic("TODO")
		}
	}
	return hf, nil
}
