// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package route

import "golang.org/x/sys/unix"

func (typ RIBType) parseable() bool {
	switch typ {
	case unix.NET_RT_STAT, unix.NET_RT_TRASH:
		return false
	default:
		return true
	}
}

// RouteMetrics represents route metrics.
type RouteMetrics struct {
	PathMTU int // path maximum transmission unit
}

// SysType implements the SysType method of Sys interface.
func (rmx *RouteMetrics) SysType() SysType { return SysMetrics }

// Sys implements the Sys method of Message interface.
func (m *RouteMessage) Sys() []Sys {
	return []Sys{
		&RouteMetrics{
			PathMTU: int(nativeEndian.Uint32(m.raw[m.extOff+4 : m.extOff+8])),
		},
	}
}

// InterfaceMetrics represents interface metrics.
type InterfaceMetrics struct {
	Type int // interface type
	MTU  int // maximum transmission unit
}

// SysType implements the SysType method of Sys interface.
func (imx *InterfaceMetrics) SysType() SysType { return SysMetrics }

// Sys implements the Sys method of Message interface.
func (m *InterfaceMessage) Sys() []Sys {
	return []Sys{
		&InterfaceMetrics{
			Type: int(m.raw[m.extOff]),
			MTU:  int(nativeEndian.Uint32(m.raw[m.extOff+8 : m.extOff+12])),
		},
	}
}

func probeRoutingStack() (int, map[int]*wireFormat) {
	rtm := &wireFormat{extOff: 36, bodyOff: unix.SizeofRtMsghdr}
	rtm.parse = rtm.parseRouteMessage
	rtm2 := &wireFormat{extOff: 36, bodyOff: sizeofRtMsghdr2Darwin15}
	rtm2.parse = rtm2.parseRouteMessage
	ifm := &wireFormat{extOff: 16, bodyOff: unix.SizeofIfMsghdr}
	ifm.parse = ifm.parseInterfaceMessage
	ifm2 := &wireFormat{extOff: 32, bodyOff: sizeofIfMsghdr2Darwin15}
	ifm2.parse = ifm2.parseInterfaceMessage
	ifam := &wireFormat{extOff: unix.SizeofIfaMsghdr, bodyOff: unix.SizeofIfaMsghdr}
	ifam.parse = ifam.parseInterfaceAddrMessage
	ifmam := &wireFormat{extOff: unix.SizeofIfmaMsghdr, bodyOff: unix.SizeofIfmaMsghdr}
	ifmam.parse = ifmam.parseInterfaceMulticastAddrMessage
	ifmam2 := &wireFormat{extOff: unix.SizeofIfmaMsghdr2, bodyOff: unix.SizeofIfmaMsghdr2}
	ifmam2.parse = ifmam2.parseInterfaceMulticastAddrMessage
	// Darwin kernels require 32-bit aligned access to routing facilities.
	return 4, map[int]*wireFormat{
		unix.RTM_ADD:       rtm,
		unix.RTM_DELETE:    rtm,
		unix.RTM_CHANGE:    rtm,
		unix.RTM_GET:       rtm,
		unix.RTM_LOSING:    rtm,
		unix.RTM_REDIRECT:  rtm,
		unix.RTM_MISS:      rtm,
		unix.RTM_LOCK:      rtm,
		unix.RTM_RESOLVE:   rtm,
		unix.RTM_NEWADDR:   ifam,
		unix.RTM_DELADDR:   ifam,
		unix.RTM_IFINFO:    ifm,
		unix.RTM_NEWMADDR:  ifmam,
		unix.RTM_DELMADDR:  ifmam,
		unix.RTM_IFINFO2:   ifm2,
		unix.RTM_NEWMADDR2: ifmam2,
		unix.RTM_GET2:      rtm2,
	}
}
