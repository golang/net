// Copyright 2014 The Go Authors.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package hpack

import (
	"bytes"
	"reflect"
	"testing"
)

func TestEncoder(t *testing.T) {
	headers := []HeaderField{
		HeaderField{Name: "content-type", Value: "text/html"},
		HeaderField{Name: "x-foo", Value: "x-bar"},
	}

	var buf bytes.Buffer
	e := NewEncoder(&buf)
	for _, hf := range headers {
		if err := e.WriteField(hf); err != nil {
			t.Fatal(err)
		}
	}

	var got []HeaderField
	_, err := NewDecoder(4<<10, func(f HeaderField) {
		got = append(got, f)
	}).Write(buf.Bytes())
	if err != nil {
		t.Error("Decoder Write = %v", err)
	}
	if !reflect.DeepEqual(got, headers) {
		t.Errorf("Decoded %+v; want %+v", got, headers)
	}

}
