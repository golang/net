// Copyright 2014 The Go Authors.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package hpack

import (
	"errors"
	"io"
)

type Encoder struct {
	w   io.Writer
	buf []byte
}

func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

// WriteField encodes f into a single Write to e's underlying Writer.
func (e *Encoder) WriteField(f HeaderField) error {
	// TODO: write a decent Encoder. This is a quick hack job just to move on
	// to other pieces. With this encoder, we only write literals.
	// The form this writes is:
	// http://http2.github.io/http2-spec/compression.html#rfc.section.6.2.2
	// In particular, that second table. And no huffman
	// compression.  Oh, and let's be even lazier for now and
	// reject keys or values with lengths greather than 126 so we
	// can save some time by not writing a varint encoder for
	// now. But I'm wasting time writing this comment.
	if len(f.Name) > 126 || len(f.Value) > 126 {
		return errors.New("TODO: finish hpack encoder. names/values too long for current laziness, procrastination, impatience.")
	}
	e.buf = e.buf[:0]
	e.buf = append(e.buf, 0)
	e.buf = append(e.buf, byte(len(f.Name)))
	e.buf = append(e.buf, f.Name...)
	e.buf = append(e.buf, byte(len(f.Value)))
	e.buf = append(e.buf, f.Value...)

	n, err := e.w.Write(e.buf)
	if err == nil && n != len(e.buf) {
		err = io.ErrShortWrite
	}
	return err
}
