// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build ignore
// +build ignore

// +godefs map struct_in_addr [4]byte /* in_addr */

package ipv4

/*
#include <sys/socket.h>

#include <netinet/in.h>
*/
import "C"

const (
	sysIP_RECVDSTADDR = C.IP_RECVDSTADDR
	sysIP_RECVIF      = C.IP_RECVIF
	sysIP_RECVTTL     = C.IP_RECVTTL

	sizeofSockaddrStorage = C.sizeof_struct_sockaddr_storage
	sizeofSockaddrInet    = C.sizeof_struct_sockaddr_in

	sizeofIPMreq         = C.sizeof_struct_ip_mreq
	sizeofIPMreqn        = C.sizeof_struct_ip_mreqn
	sizeofIPMreqSource   = C.sizeof_struct_ip_mreq_source
	sizeofGroupReq       = C.sizeof_struct_group_req
	sizeofGroupSourceReq = C.sizeof_struct_group_source_req
)

type sockaddrStorage C.struct_sockaddr_storage

type sockaddrInet C.struct_sockaddr_in

type ipMreq C.struct_ip_mreq

type ipMreqn C.struct_ip_mreqn

type ipMreqSource C.struct_ip_mreq_source

type groupReq C.struct_group_req

type groupSourceReq C.struct_group_source_req
