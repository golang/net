// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dnsmessage

import (
	"reflect"
	"testing"
)

func TestSVCBParamsRoundTrip(t *testing.T) {
	testSVCBParam := func(t *testing.T, p *SVCParam) {
		t.Helper()
		rr := &SVCBResource{
			Priority: 1,
			Target:   MustNewName("svc.example.com."),
			Params:   []SVCParam{*p},
		}
		buf, err := rr.pack([]byte{}, nil, 0)
		if err != nil {
			t.Fatalf("pack() = %v", err)
		}
		got, n, err := unpackResourceBody(buf, 0, ResourceHeader{Type: TypeSVCB, Length: uint16(len(buf))})
		if err != nil {
			t.Fatalf("unpackResourceBody() = %v", err)
		}
		if n != len(buf) {
			t.Fatalf("unpacked different amount than packed: got = %d, want = %d", n, len(buf))
		}
		if !reflect.DeepEqual(got, rr) {
			t.Fatalf("roundtrip mismatch: got = %#v, want = %#v", got, rr)
		}
	}

	testSVCBParam(t, &SVCParam{Key: SVCParamMandatory, Value: []byte{0x00, 0x01, 0x00, 0x03, 0x00, 0x05}})
	testSVCBParam(t, &SVCParam{Key: SVCParamALPN, Value: []byte{0x02, 'h', '2', 0x02, 'h', '3'}})
	testSVCBParam(t, &SVCParam{Key: SVCParamNoDefaultALPN, Value: []byte{}})
	testSVCBParam(t, &SVCParam{Key: SVCParamPort, Value: []byte{0x1f, 0x90}}) // 8080
	testSVCBParam(t, &SVCParam{Key: SVCParamIPv4Hint, Value: []byte{192, 0, 2, 1, 198, 51, 100, 2}})
	testSVCBParam(t, &SVCParam{Key: SVCParamECH, Value: []byte{0x01, 0x02, 0x03, 0x04}})
	testSVCBParam(t, &SVCParam{Key: SVCParamIPv6Hint, Value: []byte{0x20, 0x01, 0x0d, 0xb8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01}})
	testSVCBParam(t, &SVCParam{Key: SVCParamDOHPath, Value: []byte("/dns-query{?dns}")})
	testSVCBParam(t, &SVCParam{Key: SVCParamOHTTP, Value: []byte{0x00, 0x01, 0x02, 0x03}})
	testSVCBParam(t, &SVCParam{Key: SVCParamTLSSupportedGroups, Value: []byte{0x00, 0x1d, 0x00, 0x17}})
}

func TestSVCBParsingAllocs(t *testing.T) {
	name := MustNewName("foo.bar.example.com.")
	msg := Message{
		Header:    Header{Response: true, Authoritative: true},
		Questions: []Question{{Name: name, Type: TypeA, Class: ClassINET}},
		Answers: []Resource{{
			Header: ResourceHeader{Name: name, Type: TypeSVCB, Class: ClassINET, TTL: 300},
			Body: &SVCBResource{
				Priority: 1,
				Target:   MustNewName("svc.example.com."),
				Params: []SVCParam{
					{Key: SVCParamMandatory, Value: []byte{0x00, 0x01, 0x00, 0x03, 0x00, 0x05}},
					{Key: SVCParamALPN, Value: []byte{0x02, 'h', '2', 0x02, 'h', '3'}},
					{Key: SVCParamPort, Value: []byte{0x1f, 0x90}}, // 8080
					{Key: SVCParamIPv4Hint, Value: []byte{192, 0, 2, 1, 198, 51, 100, 2}},
					{Key: SVCParamIPv6Hint, Value: []byte{0x20, 0x01, 0x0d, 0xb8, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01}},
				},
			},
		}},
	}
	buf, err := msg.Pack()
	if err != nil {
		t.Fatal(err)
	}

	runParse := func() {
		t.Helper()
		var p Parser
		if _, err := p.Start(buf); err != nil {
			t.Fatal("Parser.Start(non-nil) =", err)
		}
		err := p.SkipAllQuestions()
		if err != nil {
			t.Fatal("Parser.SkipAllQuestions(non-nil) =", err)
		}
		if _, err = p.AnswerHeader(); err != nil {
			t.Fatal("Parser.AnswerHeader(non-nil) =", err)
		}
		if _, err = p.SVCBResource(); err != nil {
			t.Fatal("Parser.SVCBResource(non-nil) =", err)
		}
	}
	// Make sure we have only two allocations: one for the SVCBResource.Params slice, and one
	// for the SVCParam Values.
	if allocs := testing.AllocsPerRun(10, runParse); int(allocs) != 2 {
		t.Errorf("allocations during parsing: got = %.0f, want 2", allocs)
	}
}
