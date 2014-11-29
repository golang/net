// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipv4

import "golang.org/x/net/internal/iana"

// An ICMPType represents a type of ICMP message.
type ICMPType int

func (typ ICMPType) String() string {
	s, ok := icmpTypes[typ]
	if !ok {
		return "<nil>"
	}
	return s
}

// Protocol returns the ICMPv4 protocol number.
func (typ ICMPType) Protocol() int {
	return iana.ProtocolICMP
}
