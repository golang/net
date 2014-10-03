// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipv4

import "syscall"

type sysSockoptLen int32

const (
	sysIP_PKTINFO = 0

	sysSizeofInetPktinfo = 0xc
)

type sysInetPktinfo struct {
	Ifindex  uint32
	Spec_dst [4]byte /* in_addr */
	Addr     [4]byte /* in_addr */
}

var (
	sockOpts = [ssoMax]sockOpt{
		ssoTOS:                {sysIP_TOS, ssoTypeInt},
		ssoTTL:                {sysIP_TTL, ssoTypeInt},
		ssoMulticastTTL:       {sysIP_MULTICAST_TTL, ssoTypeByte},
		ssoMulticastInterface: {sysIP_MULTICAST_IF, ssoTypeInterface},
		ssoMulticastLoopback:  {sysIP_MULTICAST_LOOP, ssoTypeInt},
		ssoReceiveTTL:         {sysIP_RECVTTL, ssoTypeInt},
		ssoReceiveDst:         {sysIP_RECVDSTADDR, ssoTypeInt},
		ssoReceiveInterface:   {sysIP_RECVIF, ssoTypeInt},
		ssoHeaderPrepend:      {sysIP_HDRINCL, ssoTypeInt},
		ssoJoinGroup:          {sysIP_ADD_MEMBERSHIP, ssoTypeIPMreq},
		ssoLeaveGroup:         {sysIP_DROP_MEMBERSHIP, ssoTypeIPMreq},
	}
)

func init() {
	freebsdVersion, _ = syscall.SysctlUint32("kern.osreldate")
	if freebsdVersion >= 1000000 {
		sockOpts[ssoMulticastInterface].typ = ssoTypeIPMreqn
	}
}
