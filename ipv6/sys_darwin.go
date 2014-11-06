// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipv6

import (
	"net"
	"syscall"
)

type sysSockoptLen int32

var (
	ctlOpts = [ctlMax]ctlOpt{
		ctlHopLimit:   {sysIPV6_2292HOPLIMIT, 4, marshal2292HopLimit, parseHopLimit},
		ctlPacketInfo: {sysIPV6_2292PKTINFO, sysSizeofInet6Pktinfo, marshal2292PacketInfo, parsePacketInfo},
	}

	sockOpts = [ssoMax]sockOpt{
		ssoTrafficClass:       {ianaProtocolIPv6, sysIPV6_TCLASS, ssoTypeInt},
		ssoHopLimit:           {ianaProtocolIPv6, sysIPV6_UNICAST_HOPS, ssoTypeInt},
		ssoMulticastInterface: {ianaProtocolIPv6, sysIPV6_MULTICAST_IF, ssoTypeInterface},
		ssoMulticastHopLimit:  {ianaProtocolIPv6, sysIPV6_MULTICAST_HOPS, ssoTypeInt},
		ssoMulticastLoopback:  {ianaProtocolIPv6, sysIPV6_MULTICAST_LOOP, ssoTypeInt},
		ssoReceiveHopLimit:    {ianaProtocolIPv6, sysIPV6_2292HOPLIMIT, ssoTypeInt},
		ssoReceivePacketInfo:  {ianaProtocolIPv6, sysIPV6_2292PKTINFO, ssoTypeInt},
		ssoChecksum:           {ianaProtocolIPv6, sysIPV6_CHECKSUM, ssoTypeInt},
		ssoICMPFilter:         {ianaProtocolIPv6ICMP, sysICMP6_FILTER, ssoTypeICMPFilter},
		ssoJoinGroup:          {ianaProtocolIPv6, sysIPV6_JOIN_GROUP, ssoTypeIPMreq},
		ssoLeaveGroup:         {ianaProtocolIPv6, sysIPV6_LEAVE_GROUP, ssoTypeIPMreq},
	}
)

func init() {
	// Seems like kern.osreldate is veiled on latest OS X. We use
	// kern.osrelease instead.
	osver, err := syscall.Sysctl("kern.osrelease")
	if err != nil {
		return
	}
	var i int
	for i = range osver {
		if osver[i] != '.' {
			continue
		}
	}
	// The IPV6_RECVPATHMTU and IPV6_PATHMTU options were
	// introduced in OS X 10.7 (Darwin 11.0.0).
	// See http://support.apple.com/kb/HT1633.
	if i > 2 || i == 2 && osver[0] >= '1' && osver[1] >= '1' {
		ctlOpts[ctlTrafficClass].name = sysIPV6_TCLASS
		ctlOpts[ctlTrafficClass].length = 4
		ctlOpts[ctlTrafficClass].marshal = marshalTrafficClass
		ctlOpts[ctlTrafficClass].parse = parseTrafficClass
		ctlOpts[ctlHopLimit].name = sysIPV6_HOPLIMIT
		ctlOpts[ctlHopLimit].marshal = marshalHopLimit
		ctlOpts[ctlPacketInfo].name = sysIPV6_PKTINFO
		ctlOpts[ctlPacketInfo].marshal = marshalPacketInfo
		sockOpts[ssoReceiveTrafficClass].level = ianaProtocolIPv6
		sockOpts[ssoReceiveTrafficClass].name = sysIPV6_RECVTCLASS
		sockOpts[ssoReceiveTrafficClass].typ = ssoTypeInt
		sockOpts[ssoReceiveHopLimit].name = sysIPV6_RECVHOPLIMIT
		sockOpts[ssoReceivePacketInfo].name = sysIPV6_RECVPKTINFO
		sockOpts[ssoReceivePathMTU].level = ianaProtocolIPv6
		sockOpts[ssoReceivePathMTU].name = sysIPV6_RECVPATHMTU
		sockOpts[ssoReceivePathMTU].typ = ssoTypeInt
	}
}

func (sa *sysSockaddrInet6) setSockaddr(ip net.IP, i int) {
	sa.Len = sysSizeofSockaddrInet6
	sa.Family = syscall.AF_INET6
	copy(sa.Addr[:], ip)
	sa.Scope_id = uint32(i)
}

func (pi *sysInet6Pktinfo) setIfindex(i int) {
	pi.Ifindex = uint32(i)
}

func (mreq *sysIPv6Mreq) setIfindex(i int) {
	mreq.Interface = uint32(i)
}
