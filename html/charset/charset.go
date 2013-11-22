// Package charset provides common text encodings for HTML documents.
//
// The mapping from encoding labels to encodings is defined at
// http://encoding.spec.whatwg.org.
package charset

import (
	"strings"

	"code.google.com/p/go.text/encoding"
)

// Lookup returns the encoding with the specified label, and its canonical
// name. It returns nil and the empty string if label is not one of the
// standard encodings for HTML. Matching is case-insensitive and ignores
// leading and trailing whitespace.
func Lookup(label string) (e encoding.Encoding, name string) {
	label = strings.ToLower(strings.Trim(label, "\t\n\r\f "))
	enc := encodings[label]
	return enc.e, enc.name
}
