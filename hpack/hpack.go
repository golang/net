// Copyright 2014 The Go Authors.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

// Package hpack implements HPACK, a compression format for
// efficiently representing HTTP header fields in the context of HTTP/2.
//
// See http://tools.ietf.org/html/draft-ietf-httpbis-header-compression-09
package hpack

import (
	"errors"
	"fmt"
	"io"
)

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

func (d *Decoder) maxTableIndex() int {
	return len(d.dynTab.ents) + len(staticTable)
}

func (d *Decoder) at(i int) (hf HeaderField, ok bool) {
	if i < 1 {
		return
	}
	if i > d.maxTableIndex() {
		return
	}
	if i <= len(staticTable) {
		return staticTable[i-1], true
	}
	dents := d.dynTab.ents
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
			idx, size, err := readVarInt(7, p)
			if err != nil {
				return nil, err
			}
			if size == 0 {
				// TODO: will later stop processing
				// here and wait for more (buffering
				// what we've got), but this is the
				// all-at-once Decode debug version.
				return nil, io.ErrUnexpectedEOF
			}
			if idx > uint64(d.maxTableIndex()) {
				return nil, DecodingError{InvalidIndexError(idx)}
			}
			hf, ok := d.at(int(idx))
			if !ok {
				return nil, DecodingError{InvalidIndexError(idx)}
			}
			d.emit(hf, false /* TODO: sensitive ? */)
			p = p[size:]
		default:
			panic("TODO")
		}
	}
	return hf, nil
}

var errVarintOverflow = DecodingError{errors.New("varint integer overflow")}

// readVarInt reads an unsigned variable length integer off the
// beginning of p. n is the parameter as described in
// http://http2.github.io/http2-spec/compression.html#rfc.section.5.1.
//
// n must always be between 1 and 8.
//
// The returned consumed parameter is the number of bytes that were
// consumed from the beginning of p. It is zero if the end of the
// integer's representation wasn't included in p. (In this case,
// callers should wait for more data to arrive and try again with a
// larger p buffer).
func readVarInt(n byte, p []byte) (i uint64, consumed int, err error) {
	if n < 1 || n > 8 {
		panic("bad n")
	}
	if len(p) == 0 {
		return
	}
	i = uint64(p[0])
	if n < 8 {
		i &= (1 << uint64(n)) - 1
	}
	if i < (1<<uint64(n))-1 {
		return i, 1, nil
	}

	p = p[1:]
	consumed++
	var m uint64
	for len(p) > 0 {
		b := p[0]
		consumed++
		i += uint64(b&127) << m
		if b&128 == 0 {
			return
		}
		p = p[1:]
		m += 7
		if m >= 63 { // TODO: proper overflow check. making this up.
			return 0, 0, errVarintOverflow
		}
	}
	return 0, 0, nil
}
