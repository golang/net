// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package route

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

func (typ RIBType) parseable() bool { return true }

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
			PathMTU: int(nativeEndian.Uint64(m.raw[m.extOff+8 : m.extOff+16])),
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
	var p uintptr
	rtm := &wireFormat{extOff: 40, bodyOff: unix.SizeofRtMsghdr}
	rtm.parse = rtm.parseRouteMessage
	ifm := &wireFormat{extOff: 16, bodyOff: unix.SizeofIfMsghdr}
	ifm.parse = ifm.parseInterfaceMessage
	ifam := &wireFormat{extOff: unix.SizeofIfmaMsghdr, bodyOff: unix.SizeofIfaMsghdr}
	ifam.parse = ifam.parseInterfaceAddrMessage
	ifmam := &wireFormat{extOff: unix.SizeofIfmaMsghdr, bodyOff: unix.SizeofIfmaMsghdr}
	ifmam.parse = ifmam.parseInterfaceMulticastAddrMessage
	ifanm := &wireFormat{extOff: unix.SizeofIfAnnounceMsghdr, bodyOff: unix.SizeofIfAnnounceMsghdr}
	ifanm.parse = ifanm.parseInterfaceAnnounceMessage

	rel, _ := unix.SysctlUint32("kern.osreldate")
	if rel < 500705 {
		// https://github.com/DragonFlyBSD/DragonFlyBSD/commit/43a373152df2d405c9940983e584e6a25e76632d
		// but only the size of struct ifa_msghdr actually changed.
		// The type is not in current header files,
		// so we just use constants here.
		rtmVersion = 7
		ifam.bodyOff = 0x14
	}

	return int(unsafe.Sizeof(p)), map[int]*wireFormat{
		unix.RTM_ADD:        rtm,
		unix.RTM_DELETE:     rtm,
		unix.RTM_CHANGE:     rtm,
		unix.RTM_GET:        rtm,
		unix.RTM_LOSING:     rtm,
		unix.RTM_REDIRECT:   rtm,
		unix.RTM_MISS:       rtm,
		unix.RTM_LOCK:       rtm,
		unix.RTM_RESOLVE:    rtm,
		unix.RTM_NEWADDR:    ifam,
		unix.RTM_DELADDR:    ifam,
		unix.RTM_IFINFO:     ifm,
		unix.RTM_NEWMADDR:   ifmam,
		unix.RTM_DELMADDR:   ifmam,
		unix.RTM_IFANNOUNCE: ifanm,
	}
}
