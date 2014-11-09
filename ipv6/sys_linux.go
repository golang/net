// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipv6

import (
	"net"
	"syscall"

	"golang.org/x/net/internal/iana"
)

type sysSockoptLen int32

var (
	ctlOpts = [ctlMax]ctlOpt{
		ctlTrafficClass: {sysIPV6_TCLASS, 4, marshalTrafficClass, parseTrafficClass},
		ctlHopLimit:     {sysIPV6_HOPLIMIT, 4, marshalHopLimit, parseHopLimit},
		ctlPacketInfo:   {sysIPV6_PKTINFO, sysSizeofInet6Pktinfo, marshalPacketInfo, parsePacketInfo},
	}

	sockOpts = [ssoMax]sockOpt{
		ssoTrafficClass:        {iana.ProtocolIPv6, sysIPV6_TCLASS, ssoTypeInt},
		ssoHopLimit:            {iana.ProtocolIPv6, sysIPV6_UNICAST_HOPS, ssoTypeInt},
		ssoMulticastInterface:  {iana.ProtocolIPv6, sysIPV6_MULTICAST_IF, ssoTypeInterface},
		ssoMulticastHopLimit:   {iana.ProtocolIPv6, sysIPV6_MULTICAST_HOPS, ssoTypeInt},
		ssoMulticastLoopback:   {iana.ProtocolIPv6, sysIPV6_MULTICAST_LOOP, ssoTypeInt},
		ssoReceiveTrafficClass: {iana.ProtocolIPv6, sysIPV6_RECVTCLASS, ssoTypeInt},
		ssoReceiveHopLimit:     {iana.ProtocolIPv6, sysIPV6_RECVHOPLIMIT, ssoTypeInt},
		ssoReceivePacketInfo:   {iana.ProtocolIPv6, sysIPV6_RECVPKTINFO, ssoTypeInt},
		ssoReceivePathMTU:      {iana.ProtocolIPv6, sysIPV6_RECVPATHMTU, ssoTypeInt},
		ssoPathMTU:             {iana.ProtocolIPv6, sysIPV6_PATHMTU, ssoTypeMTUInfo},
		ssoChecksum:            {iana.ProtocolReserved, sysIPV6_CHECKSUM, ssoTypeInt},
		ssoICMPFilter:          {iana.ProtocolIPv6ICMP, sysICMPV6_FILTER, ssoTypeICMPFilter},
		ssoJoinGroup:           {iana.ProtocolIPv6, sysIPV6_ADD_MEMBERSHIP, ssoTypeIPMreq},
		ssoLeaveGroup:          {iana.ProtocolIPv6, sysIPV6_DROP_MEMBERSHIP, ssoTypeIPMreq},
	}
)

func (sa *sysSockaddrInet6) setSockaddr(ip net.IP, i int) {
	sa.Family = syscall.AF_INET6
	copy(sa.Addr[:], ip)
	sa.Scope_id = uint32(i)
}

func (pi *sysInet6Pktinfo) setIfindex(i int) {
	pi.Ifindex = int32(i)
}

func (mreq *sysIPv6Mreq) setIfindex(i int) {
	mreq.Ifindex = int32(i)
}
