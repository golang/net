// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipv4_test

import (
	"bytes"
	"flag"
)

var testExternal = flag.Bool("external", true, "allow use of external networks during long test")

func newICMPEchoRequest(id, seqnum, msglen int, filler []byte) []byte {
	b := newICMPInfoMessage(id, seqnum, msglen, filler)
	b[0] = 8
	// calculate ICMP checksum
	cklen := len(b)
	s := uint32(0)
	for i := 0; i < cklen-1; i += 2 {
		s += uint32(b[i+1])<<8 | uint32(b[i])
	}
	if cklen&1 == 1 {
		s += uint32(b[cklen-1])
	}
	s = (s >> 16) + (s & 0xffff)
	s = s + (s >> 16)
	// place checksum back in header; using ^= avoids the
	// assumption the checksum bytes are zero
	b[2] ^= byte(^s & 0xff)
	b[3] ^= byte(^s >> 8)
	return b
}

func newICMPInfoMessage(id, seqnum, msglen int, filler []byte) []byte {
	b := make([]byte, msglen)
	copy(b[8:], bytes.Repeat(filler, (msglen-8)/len(filler)+1))
	b[0] = 0                   // type
	b[1] = 0                   // code
	b[2] = 0                   // checksum
	b[3] = 0                   // checksum
	b[4] = byte(id >> 8)       // identifier
	b[5] = byte(id & 0xff)     // identifier
	b[6] = byte(seqnum >> 8)   // sequence number
	b[7] = byte(seqnum & 0xff) // sequence number
	return b
}
