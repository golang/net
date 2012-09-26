// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipv4

import (
	"fmt"
	"net"
	"sync"
)

type rawOpt struct {
	mu     sync.Mutex
	cflags ControlFlags
}

func (o *rawOpt) lock()                     { o.mu.Lock() }
func (o *rawOpt) unlock()                   { o.mu.Unlock() }
func (o *rawOpt) set(f ControlFlags)        { o.cflags |= f }
func (o *rawOpt) clear(f ControlFlags)      { o.cflags ^= f }
func (o *rawOpt) isset(f ControlFlags) bool { return o.cflags&f != 0 }

type ControlFlags uint

const (
	FlagTTL       ControlFlags = 1 << iota // pass the TTL on the received packet
	FlagSrc                                // pass the source address on the received packet
	FlagDst                                // pass the destination address on the received packet
	FlagInterface                          // pass the interface index on the received packet or outgoing packet
)

// A ControlMessage represents control information that contains per
// packet IP-level option data.
type ControlMessage struct {
	TTL     int    // time-to-live
	Src     net.IP // source address
	Dst     net.IP // destination address
	IfIndex int    // interface index
}

func (cm *ControlMessage) String() string {
	if cm == nil {
		return "<nil>"
	}
	return fmt.Sprintf("ttl: %v, src: %v, dst: %v, ifindex: %v", cm.TTL, cm.Src, cm.Dst, cm.IfIndex)
}
