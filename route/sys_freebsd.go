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
	if kernelAlign == 8 {
		return []Sys{
			&RouteMetrics{
				PathMTU: int(nativeEndian.Uint64(m.raw[m.extOff+8 : m.extOff+16])),
			},
		}
	}
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

var compatFreeBSD32 bool // 386 emulation on amd64

func probeRoutingStack() (int, map[int]*wireFormat) {
	var p uintptr
	wordSize := int(unsafe.Sizeof(p))
	align := wordSize
	// In the case of kern.supported_archs="amd64 i386", we need
	// to know the underlying kernel's architecture because the
	// alignment for routing facilities are set at the build time
	// of the kernel.
	conf, _ := unix.Sysctl("kern.conftxt")
	for i, j := 0, 0; j < len(conf); j++ {
		if conf[j] != '\n' {
			continue
		}
		s := conf[i:j]
		i = j + 1
		if len(s) > len("machine") && s[:len("machine")] == "machine" {
			s = s[len("machine"):]
			for k := 0; k < len(s); k++ {
				if s[k] == ' ' || s[k] == '\t' {
					s = s[1:]
				}
				break
			}
			if s == "amd64" {
				align = 8
			}
			break
		}
	}
	if align != wordSize {
		compatFreeBSD32 = true // 386 emulation on amd64
	}
	var rtm *wireFormat
	ifam := &wireFormat{extOff: unix.SizeofIfaMsghdr, bodyOff: unix.SizeofIfaMsghdr}
	ifmam := &wireFormat{extOff: unix.SizeofIfmaMsghdr, bodyOff: unix.SizeofIfmaMsghdr}
	ifanm := &wireFormat{extOff: unix.SizeofIfAnnounceMsghdr, bodyOff: unix.SizeofIfAnnounceMsghdr}
	ifm := &wireFormat{extOff: 16}
	if compatFreeBSD32 {
		rtm = &wireFormat{extOff: sizeofRtMsghdrFreeBSD10Emu - sizeofRtMetricsFreeBSD10Emu, bodyOff: sizeofRtMsghdrFreeBSD10Emu}
	} else {
		rtm = &wireFormat{extOff: sizeofRtMsghdrFreeBSD10 - sizeofRtMetricsFreeBSD10, bodyOff: sizeofRtMsghdrFreeBSD10}
	}
	rel, _ := unix.SysctlUint32("kern.osreldate")
	switch {
	case rel < 800000:
		if compatFreeBSD32 {
			ifm.bodyOff = sizeofIfMsghdrFreeBSD7Emu
		} else {
			ifm.bodyOff = sizeofIfMsghdrFreeBSD7
		}
	case 800000 <= rel && rel < 900000:
		if compatFreeBSD32 {
			ifm.bodyOff = sizeofIfMsghdrFreeBSD8Emu
		} else {
			ifm.bodyOff = sizeofIfMsghdrFreeBSD8
		}
	case 900000 <= rel && rel < 1000000:
		if compatFreeBSD32 {
			ifm.bodyOff = sizeofIfMsghdrFreeBSD9Emu
		} else {
			ifm.bodyOff = sizeofIfMsghdrFreeBSD9
		}
	case 1000000 <= rel && rel < 1100000:
		if compatFreeBSD32 {
			ifm.bodyOff = sizeofIfMsghdrFreeBSD10Emu
		} else {
			ifm.bodyOff = sizeofIfMsghdrFreeBSD10
		}
	default:
		if compatFreeBSD32 {
			ifm.bodyOff = sizeofIfMsghdrFreeBSD11Emu
		} else {
			ifm.bodyOff = sizeofIfMsghdrFreeBSD11
		}
	}
	rtm.parse = rtm.parseRouteMessage
	ifm.parse = ifm.parseInterfaceMessage
	ifam.parse = ifam.parseInterfaceAddrMessage
	ifmam.parse = ifmam.parseInterfaceMulticastAddrMessage
	ifanm.parse = ifanm.parseInterfaceAnnounceMessage
	return align, map[int]*wireFormat{
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
