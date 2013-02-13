// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipv4

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	errMissingAddress  = errors.New("missing address")
	errMissingHeader   = errors.New("missing header")
	errHeaderTooShort  = errors.New("header too short")
	errBufferTooShort  = errors.New("buffer too short")
	errInvalidConnType = errors.New("invalid conn type")
)

// References:
//
// RFC  791  Internet Protocol
//	http://tools.ietf.org/html/rfc791
// RFC 1112  Host Extensions for IP Multicasting
//	http://tools.ietf.org/html/rfc1112
// RFC 1122  Requirements for Internet Hosts
//	http://tools.ietf.org/html/rfc1122
// RFC 2474  Definition of the Differentiated Services Field (DS Field) in the IPv4 and IPv6 Headers
//	http://tools.ietf.org/html/rfc2474
// RFC 2475  An Architecture for Differentiated Services
//	http://tools.ietf.org/html/rfc2475
// RFC 2597  Assured Forwarding PHB Group
//	http://tools.ietf.org/html/rfc2597
// RFC 2598  An Expedited Forwarding PHB
//	http://tools.ietf.org/html/rfc2598
// RFC 3168  The Addition of Explicit Congestion Notification (ECN) to IP
//	http://tools.ietf.org/html/rfc3168
// RFC 3260  New Terminology and Clarifications for Diffserv
//	http://tools.ietf.org/html/rfc3260

const (
	Version      = 4  // protocol version
	HeaderLen    = 20 // header length without extension headers
	maxHeaderLen = 60 // sensible default, revisit if later RFCs define new usage of version and header length fields
)

const (
	// DiffServ class selector codepoints in RFC 2474.
	DSCP_CS0 = 0x00 // best effort
	DSCP_CS1 = 0x20 // class 1
	DSCP_CS2 = 0x40 // class 2
	DSCP_CS3 = 0x60 // class 3
	DSCP_CS4 = 0x80 // class 4
	DSCP_CS5 = 0xa0 // expedited forwarding
	DSCP_CS6 = 0xc0 // subsume deprecated IP precedence, internet control (routing information update)
	DSCP_CS7 = 0xe0 // subsume deprecated IP precedence, network control (link, neighbor liveliness check)

	// DiffServ assured forwarding codepoints in RFC 2474, 2475, 2597 and 3260.
	DSCP_AF11 = 0x28 // class 1 low drop precedence
	DSCP_AF12 = 0x30 // class 1 medium drop precedence
	DSCP_AF13 = 0x38 // class 1 high drop precedence
	DSCP_AF21 = 0x48 // class 2 low drop precedence
	DSCP_AF22 = 0x50 // class 2 medium drop precedence
	DSCP_AF23 = 0x58 // class 2 high drop precedence
	DSCP_AF31 = 0x68 // class 3 low drop precedence
	DSCP_AF32 = 0x70 // class 3 medium drop precedence
	DSCP_AF33 = 0x78 // class 3 high drop precedence
	DSCP_AF41 = 0x88 // class 4 low drop precedence
	DSCP_AF42 = 0x90 // class 4 medium drop precedence
	DSCP_AF43 = 0x98 // class 4 high drop precedence
	DSCP_EF   = 0xb8 // expedited forwarding

	// ECN codepoints in RFC 3168.
	ECN_NOTECT = 0x00 // not ECN-capable transport
	ECN_ECT1   = 0x01 // ECN-capable transport, ECT(1)
	ECN_ECT0   = 0x02 // ECN-capable transport, ECT(0)
	ECN_CE     = 0x03 // congestion experienced
)

type headerField int

const (
	posTOS      headerField = 1  // type-of-service
	posTotalLen             = 2  // packet total length
	posID                   = 4  // identification
	posFragOff              = 6  // fragment offset
	posTTL                  = 8  // time-to-live
	posProtocol             = 9  // next protocol
	posChecksum             = 10 // checksum
	posSrc                  = 12 // source address
	posDst                  = 16 // destination address
)

// A Header represents an IPv4 header.
type Header struct {
	Version  int    // protocol version
	Len      int    // header length
	TOS      int    // type-of-service
	TotalLen int    // packet total length
	ID       int    // identification
	FragOff  int    // fragment offset
	TTL      int    // time-to-live
	Protocol int    // next protocol
	Checksum int    // checksum
	Src      net.IP // source address
	Dst      net.IP // destination address
	Options  []byte // options, extension headers
}

func (h *Header) String() string {
	if h == nil {
		return "<nil>"
	}
	return fmt.Sprintf("ver: %v, hdrlen: %v, tos: %#x, totallen: %v, id: %#x, fragoff: %#x, ttl: %v, proto: %v, cksum: %#x, src: %v, dst: %v", h.Version, h.Len, h.TOS, h.TotalLen, h.ID, h.FragOff, h.TTL, h.Protocol, h.Checksum, h.Src, h.Dst)
}

// Please refer to the online manual; IP(4) on Darwin, FreeBSD and
// OpenBSD.  IP(7) on Linux.
const supportsNewIPInput = runtime.GOOS == "linux" || runtime.GOOS == "openbsd"

// Marshal returns the binary encoding of the IPv4 header h.
func (h *Header) Marshal() ([]byte, error) {
	if h == nil {
		return nil, syscall.EINVAL
	}
	if h.Len < HeaderLen {
		return nil, errHeaderTooShort
	}
	hdrlen := HeaderLen + len(h.Options)
	b := make([]byte, hdrlen)
	b[0] = byte(Version<<4 | (hdrlen >> 2 & 0x0f))
	b[posTOS] = byte(h.TOS)
	if supportsNewIPInput {
		b[posTotalLen], b[posTotalLen+1] = byte(h.TotalLen>>8), byte(h.TotalLen)
		b[posFragOff], b[posFragOff+1] = byte(h.FragOff>>8), byte(h.FragOff)
	} else {
		*(*uint16)(unsafe.Pointer(&b[posTotalLen : posTotalLen+1][0])) = uint16(h.TotalLen)
		*(*uint16)(unsafe.Pointer(&b[posFragOff : posFragOff+1][0])) = uint16(h.FragOff)
	}
	b[posID], b[posID+1] = byte(h.ID>>8), byte(h.ID)
	b[posTTL] = byte(h.TTL)
	b[posProtocol] = byte(h.Protocol)
	b[posChecksum], b[posChecksum+1] = byte(h.Checksum>>8), byte(h.Checksum)
	if ip := h.Src.To4(); ip != nil {
		copy(b[posSrc:posSrc+net.IPv4len], ip[:net.IPv4len])
	}
	if ip := h.Dst.To4(); ip != nil {
		copy(b[posDst:posDst+net.IPv4len], ip[:net.IPv4len])
	} else {
		return nil, errMissingAddress
	}
	if len(h.Options) > 0 {
		copy(b[HeaderLen:], h.Options)
	}
	return b, nil
}

// ParseHeader parses b as an IPv4 header.
func ParseHeader(b []byte) (*Header, error) {
	if len(b) < HeaderLen {
		return nil, errHeaderTooShort
	}
	hdrlen := int(b[0]&0x0f) << 2
	if hdrlen > len(b) {
		return nil, errBufferTooShort
	}
	h := &Header{}
	h.Version = int(b[0] >> 4)
	h.Len = hdrlen
	h.TOS = int(b[posTOS])
	if supportsNewIPInput {
		h.TotalLen = int(b[posTotalLen])<<8 | int(b[posTotalLen+1])
		h.FragOff = int(b[posFragOff])<<8 | int(b[posFragOff+1])
	} else {
		h.TotalLen = int(*(*uint16)(unsafe.Pointer(&b[posTotalLen : posTotalLen+1][0])))
		h.TotalLen += hdrlen
		h.FragOff = int(*(*uint16)(unsafe.Pointer(&b[posFragOff : posFragOff+1][0])))
	}
	h.ID = int(b[posID])<<8 | int(b[posID+1])
	h.TTL = int(b[posTTL])
	h.Protocol = int(b[posProtocol])
	h.Checksum = int(b[posChecksum])<<8 | int(b[posChecksum+1])
	h.Src = net.IPv4(b[posSrc], b[posSrc+1], b[posSrc+2], b[posSrc+3])
	h.Dst = net.IPv4(b[posDst], b[posDst+1], b[posDst+2], b[posDst+3])
	if hdrlen-HeaderLen > 0 {
		h.Options = make([]byte, hdrlen-HeaderLen)
		copy(h.Options, b[HeaderLen:])
	}
	return h, nil
}
