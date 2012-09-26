// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipv4_test

import (
	"bytes"
	"code.google.com/p/go.net/ipv4"
	"net"
	"reflect"
	"runtime"
	"testing"
)

var (
	wireHeaderFromKernel = [ipv4.HeaderLen]byte{
		0x45, 0x01, 0xbe, 0xef,
		0xca, 0xfe, 0x05, 0xdc,
		0xff, 0x01, 0xde, 0xad,
		172, 16, 254, 254,
		192, 168, 0, 1,
	}
	wireHeaderToKernel = [ipv4.HeaderLen]byte{
		0x45, 0x01, 0xbe, 0xef,
		0xca, 0xfe, 0x05, 0xdc,
		0xff, 0x01, 0xde, 0xad,
		172, 16, 254, 254,
		192, 168, 0, 1,
	}
	wireHeaderFromTradBSDKernel = [ipv4.HeaderLen]byte{
		0x45, 0x01, 0xdb, 0xbe,
		0xca, 0xfe, 0xdc, 0x05,
		0xff, 0x01, 0xde, 0xad,
		172, 16, 254, 254,
		192, 168, 0, 1,
	}
	wireHeaderToTradBSDKernel = [ipv4.HeaderLen]byte{
		0x45, 0x01, 0xef, 0xbe,
		0xca, 0xfe, 0xdc, 0x05,
		0xff, 0x01, 0xde, 0xad,
		172, 16, 254, 254,
		192, 168, 0, 1,
	}
	// TODO(mikio): Add platform dependent wire header formats when
	// we support new platforms.
)

func testHeader() *ipv4.Header {
	h := &ipv4.Header{}
	h.Version = ipv4.Version
	h.Len = ipv4.HeaderLen
	h.TOS = 1
	h.TotalLen = 0xbeef
	h.ID = 0xcafe
	h.FragOff = 1500
	h.TTL = 255
	h.Protocol = 1
	h.Checksum = 0xdead
	h.Src = net.IPv4(172, 16, 254, 254)
	h.Dst = net.IPv4(192, 168, 0, 1)
	return h
}

func TestMarshalHeader(t *testing.T) {
	th := testHeader()
	b, err := th.Marshal()
	if err != nil {
		t.Fatalf("ipv4.Header.Marshal failed: %v", err)
	}
	var wh []byte
	switch runtime.GOOS {
	case "linux", "openbsd":
		wh = wireHeaderToKernel[:]
	default:
		wh = wireHeaderToTradBSDKernel[:]
	}
	if !bytes.Equal(b, wh) {
		t.Fatalf("ipv4.Header.Marshal failed: %#v not equal %#v", b, wh)
	}
}

func TestParseHeader(t *testing.T) {
	var wh []byte
	switch runtime.GOOS {
	case "linux", "openbsd":
		wh = wireHeaderFromKernel[:]
	default:
		wh = wireHeaderFromTradBSDKernel[:]
	}
	h, err := ipv4.ParseHeader(wh)
	if err != nil {
		t.Fatalf("ipv4.ParseHeader failed: %v", err)
	}
	th := testHeader()
	if !reflect.DeepEqual(h, th) {
		t.Fatalf("ipv4.ParseHeader failed: %#v not equal %#v", h, th)
	}
}
