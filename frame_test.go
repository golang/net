// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"reflect"
	"testing"
)

func testFramer() (*Framer, *bytes.Buffer) {
	buf := new(bytes.Buffer)
	return NewFramer(buf, buf), buf
}

func TestWriteRST(t *testing.T) {
	fr, buf := testFramer()
	var streamID uint32 = 1<<24 + 2<<16 + 3<<8 + 4
	var errCode uint32 = 7<<24 + 6<<16 + 5<<8 + 4
	fr.WriteRSTStream(streamID, errCode)
	const wantEnc = "\x00\x00\x04\x03\x00\x01\x02\x03\x04\x07\x06\x05\x04"
	if buf.String() != wantEnc {
		t.Errorf("encoded as %q; want %q", buf.Bytes(), wantEnc)
	}
	f, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	want := &RSTStreamFrame{
		FrameHeader: FrameHeader{
			valid:    true,
			Type:     0x3,
			Flags:    0x0,
			Length:   0x4,
			StreamID: 0x1020304,
		},
		ErrCode: 0x7060504,
	}
	if !reflect.DeepEqual(f, want) {
		t.Errorf("parsed back %#v; want %#v", f, want)
	}
}

func TestWriteData(t *testing.T) {
	fr, buf := testFramer()
	var streamID uint32 = 1<<24 + 2<<16 + 3<<8 + 4
	data := []byte("ABC")
	fr.WriteData(streamID, true, data)
	const wantEnc = "\x00\x00\x03\x00\x01\x01\x02\x03\x04ABC"
	if buf.String() != wantEnc {
		t.Errorf("encoded as %q; want %q", buf.Bytes(), wantEnc)
	}
	f, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	df, ok := f.(*DataFrame)
	if !ok {
		t.Fatalf("got %T; want *DataFrame", f)
	}
	if !bytes.Equal(df.Data(), data) {
		t.Errorf("got %q; want %q", df.Data, data)
	}
	if f.Header().Flags&1 == 0 {
		t.Errorf("didn't see END_STREAM flag")
	}
}
