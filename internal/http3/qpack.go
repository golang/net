// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.24

package http3

import (
	"io"

	"golang.org/x/net/http2/hpack"
)

// Prefixed-integer encoding from RFC 7541, section 5.1
//
// Prefixed integers consist of some number of bits of data,
// N bits of encoded integer, and 0 or more additional bytes of
// encoded integer.
//
// The RFCs represent this as, for example:
//
//       0   1   2   3   4   5   6   7
//     +---+---+---+---+---+---+---+---+
//     | 0 | 0 | 1 |   Capacity (5+)   |
//     +---+---+---+-------------------+
//
// "Capacity" is an integer with a 5-bit prefix.
//
// In the following functions, a "prefixLen" parameter is the number
// of integer bits in the first byte (5 in the above example), and
// a "firstByte" parameter is a byte containing the first byte of
// the encoded value (0x001x_xxxx in the above example).
//
// https://www.rfc-editor.org/rfc/rfc9204.html#section-4.1.1
// https://www.rfc-editor.org/rfc/rfc7541#section-5.1

// readPrefixedInt reads an RFC 7541 prefixed integer from st.
func (st *stream) readPrefixedInt(prefixLen uint8) (firstByte byte, v int64, err error) {
	firstByte, err = st.ReadByte()
	if err != nil {
		return 0, 0, errQPACKDecompressionFailed
	}
	v, err = st.readPrefixedIntWithByte(firstByte, prefixLen)
	return firstByte, v, err
}

// readPrefixedInt reads an RFC 7541 prefixed integer from st.
// The first byte has already been read from the stream.
func (st *stream) readPrefixedIntWithByte(firstByte byte, prefixLen uint8) (v int64, err error) {
	prefixMask := (byte(1) << prefixLen) - 1
	v = int64(firstByte & prefixMask)
	if v != int64(prefixMask) {
		return v, nil
	}
	m := 0
	for {
		b, err := st.ReadByte()
		if err != nil {
			return 0, errQPACKDecompressionFailed
		}
		v += int64(b&127) << m
		m += 7
		if b&128 == 0 {
			break
		}
	}
	return v, err
}

// appendPrefixedInt appends an RFC 7541 prefixed integer to b.
//
// The firstByte parameter includes the non-integer bits of the first byte.
// The other bits must be zero.
func appendPrefixedInt(b []byte, firstByte byte, prefixLen uint8, i int64) []byte {
	u := uint64(i)
	prefixMask := (uint64(1) << prefixLen) - 1
	if u < prefixMask {
		return append(b, firstByte|byte(u))
	}
	b = append(b, firstByte|byte(prefixMask))
	u -= prefixMask
	for u >= 128 {
		b = append(b, 0x80|byte(u&0x7f))
		u >>= 7
	}
	return append(b, byte(u))
}

// String literal encoding from RFC 7541, section 5.2
//
// String literals consist of a single bit flag indicating
// whether the string is Huffman-encoded, a prefixed integer (see above),
// and the string.
//
// https://www.rfc-editor.org/rfc/rfc9204.html#section-4.1.2
// https://www.rfc-editor.org/rfc/rfc7541#section-5.2

// readPrefixedString reads an RFC 7541 string from st.
func (st *stream) readPrefixedString(prefixLen uint8) (firstByte byte, s string, err error) {
	firstByte, err = st.ReadByte()
	if err != nil {
		return 0, "", errQPACKDecompressionFailed
	}
	s, err = st.readPrefixedStringWithByte(firstByte, prefixLen)
	return firstByte, s, err
}

// readPrefixedString reads an RFC 7541 string from st.
// The first byte has already been read from the stream.
func (st *stream) readPrefixedStringWithByte(firstByte byte, prefixLen uint8) (s string, err error) {
	size, err := st.readPrefixedIntWithByte(firstByte, prefixLen)
	if err != nil {
		return "", errQPACKDecompressionFailed
	}

	hbit := byte(1) << prefixLen
	isHuffman := firstByte&hbit != 0

	// TODO: Avoid allocating here.
	data := make([]byte, size)
	if _, err := io.ReadFull(st, data); err != nil {
		return "", errQPACKDecompressionFailed
	}
	if isHuffman {
		// TODO: Move Huffman functions into a new package that hpack (HTTP/2)
		// and this package can both import. Most of the hpack package isn't
		// relevant to HTTP/3.
		s, err := hpack.HuffmanDecodeToString(data)
		if err != nil {
			return "", errQPACKDecompressionFailed
		}
		return s, nil
	}
	return string(data), nil
}

// appendPrefixedString appends an RFC 7541 string to st.
//
// The firstByte parameter includes the non-integer bits of the first byte.
// The other bits must be zero.
func appendPrefixedString(b []byte, firstByte byte, prefixLen uint8, s string) []byte {
	huffmanLen := hpack.HuffmanEncodeLength(s)
	if huffmanLen < uint64(len(s)) {
		hbit := byte(1) << prefixLen
		b = appendPrefixedInt(b, firstByte|hbit, prefixLen, int64(huffmanLen))
		b = hpack.AppendHuffmanString(b, s)
	} else {
		b = appendPrefixedInt(b, firstByte, prefixLen, int64(len(s)))
		b = append(b, s...)
	}
	return b
}
