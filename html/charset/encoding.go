// Package charset provides common text encodings for HTML documents.
//
// The mapping from encoding labels to encodings is defined at
// http://encoding.spec.whatwg.org.
package charset

import (
	"errors"
	"strings"

	"code.google.com/p/go.text/encoding"
	"code.google.com/p/go.text/transform"
)

// Encoding returns the encoding with the specified label, or nil if label is
// not one of the standard encodings for HTML. Matching is case-insensitive
// and ignores leading and trailing whitespace.
func Encoding(label string) encoding.Encoding {
	label = strings.ToLower(strings.Trim(label, "\t\n\r\f "))
	return encodings[label]
}

type utf8Encoding struct{}

func (utf8Encoding) NewDecoder() transform.Transformer {
	return transform.Nop
}

func (utf8Encoding) NewEncoder() transform.Transformer {
	return transform.Nop
}

var ErrReplacementEncoding = errors.New("charset: ISO-2022-CN and ISO-2022-KR are obsolete encodings for HTML")

type errorTransformer struct {
	err error
}

func (t errorTransformer) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	return 0, 0, t.err
}

type replacementEncoding struct{}

func (replacementEncoding) NewDecoder() transform.Transformer {
	return errorTransformer{ErrReplacementEncoding}
}

func (replacementEncoding) NewEncoder() transform.Transformer {
	return transform.Nop
}
