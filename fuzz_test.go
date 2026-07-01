//go:build go1.18
// +build go1.18

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package net_test

import (
	"bytes"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/http2/hpack"
)

// FuzzDNSMessageParse tests DNS message parsing with arbitrary byte inputs.
// DNS is critical internet infrastructure — a parsing bug here affects
// every DNS resolver built in Go.
func FuzzDNSMessageParse(f *testing.F) {
	f.Add([]byte{
		0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0x03, 'c', 'o', 'm', 0x00,
		0x00, 0x01, 0x00, 0x01,
	})
	f.Add([]byte{})
	f.Add([]byte{0x00, 0x01, 0x02, 0x03})

	f.Fuzz(func(t *testing.T, data []byte) {
		var p dnsmessage.Parser
		h, err := p.Start(data)
		if err != nil {
			return
		}
		_ = h

		for {
			q, err := p.Question()
			if err != nil {
				break
			}
			_ = q
		}

		for {
			ah, err := p.AnswerHeader()
			if err != nil {
				break
			}
			_, _ = p.Answer()
			_ = ah
		}
	})
}

// FuzzHpackDecode tests HTTP/2 HPACK header decoding with arbitrary binary input.
// HPACK is used by every HTTP/2 connection. A crash here is a DoS vector
// for any Go HTTP/2 server.
func FuzzHpackDecode(f *testing.F) {
	f.Add([]byte{
		0x82,
		0x86,
		0x84,
		0x0f, 0x02, 0x03, 'f', 'o', 'o',
	})
	f.Add([]byte{})
	f.Add([]byte{0x80})
	f.Add([]byte{0x3f, 0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		var buf bytes.Buffer
		dec := hpack.NewDecoder(4096, func(f hpack.HeaderField) {
			buf.WriteString(f.Name)
			buf.WriteString(f.Value)
		})

		_, _ = dec.Write(data)
		_ = dec.Close()
	})
}

// FuzzHpackRoundTrip tests HPACK encode → decode consistency.
func FuzzHpackRoundTrip(f *testing.F) {
	f.Add("content-type", "text/html")
	f.Add("x-custom", "value")
	f.Add(":method", "GET")

	f.Fuzz(func(t *testing.T, name, value string) {
		var buf bytes.Buffer
		enc := hpack.NewEncoder(&buf)
		err := enc.WriteField(hpack.HeaderField{Name: name, Value: value, Sensitive: false})
		if err != nil {
			return
		}

		encoded := buf.Bytes()
		decoded := false
		dec := hpack.NewDecoder(4096, func(f hpack.HeaderField) {
			decoded = true
		})

		_, err = dec.Write(encoded)
		if err != nil {
			return
		}
		_ = decoded
	})
}
