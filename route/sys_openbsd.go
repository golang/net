// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package route

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

func (typ RIBType) parseable() bool {
	switch typ {
	case unix.NET_RT_STATS, unix.NET_RT_TABLE:
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
			PathMTU: int(nativeEndian.Uint32(m.raw[60:64])),
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
			Type: int(m.raw[24]),
			MTU:  int(nativeEndian.Uint32(m.raw[28:32])),
		},
	}
}

func probeRoutingStack() (int, map[int]*wireFormat) {
	var p uintptr
	rtm := &wireFormat{extOff: -1, bodyOff: -1}
	rtm.parse = rtm.parseRouteMessage
	ifm := &wireFormat{extOff: -1, bodyOff: -1}
	ifm.parse = ifm.parseInterfaceMessage
	ifam := &wireFormat{extOff: -1, bodyOff: -1}
	ifam.parse = ifam.parseInterfaceAddrMessage
	ifanm := &wireFormat{extOff: -1, bodyOff: -1}
	ifanm.parse = ifanm.parseInterfaceAnnounceMessage
	return int(unsafe.Sizeof(p)), map[int]*wireFormat{
		unix.RTM_ADD:        rtm,
		unix.RTM_DELETE:     rtm,
		unix.RTM_CHANGE:     rtm,
		unix.RTM_GET:        rtm,
		unix.RTM_LOSING:     rtm,
		unix.RTM_REDIRECT:   rtm,
		unix.RTM_MISS:       rtm,
		unix.RTM_RESOLVE:    rtm,
		unix.RTM_NEWADDR:    ifam,
		unix.RTM_DELADDR:    ifam,
		unix.RTM_IFINFO:     ifm,
		unix.RTM_IFANNOUNCE: ifanm,
		unix.RTM_DESYNC:     rtm,
	}
}
