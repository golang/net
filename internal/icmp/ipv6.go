// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package icmp

import (
	"net"

	"code.google.com/p/go.net/internal/iana"
)

const ipv6PseudoHeaderLen = 2*net.IPv6len + 8

// IPv6PseudoHeader returns an IPv6 pseudo header for checkusm
// calculation.
func IPv6PseudoHeader(src, dst net.IP) []byte {
	b := make([]byte, ipv6PseudoHeaderLen)
	copy(b[:net.IPv6len], src)
	copy(b[net.IPv6len:], dst)
	b[len(b)-1] = byte(iana.ProtocolIPv6ICMP)
	return b
}
