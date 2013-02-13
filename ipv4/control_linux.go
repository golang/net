// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipv4

import (
	"net"
	"os"
	"syscall"
	"unsafe"
)

// Linux provides a convenient path control option IP_PKTINFO that
// contains IP_SENDSRCADDR, IP_RECVDSTADDR, IP_RECVIF and IP_SENDIF.
const pktinfo = FlagSrc | FlagDst | FlagInterface

func setControlMessage(fd int, opt *rawOpt, cf ControlFlags, on bool) error {
	opt.lock()
	defer opt.unlock()
	if cf&FlagTTL != 0 {
		if err := setIPv4ReceiveTTL(fd, on); err != nil {
			return err
		}
		if on {
			opt.set(FlagTTL)
		} else {
			opt.clear(FlagTTL)
		}
	}
	if cf&pktinfo != 0 {
		if err := setIPv4PacketInfo(fd, on); err != nil {
			return err
		}
		if on {
			opt.set(cf & pktinfo)
		} else {
			opt.clear(cf & pktinfo)
		}
	}
	return nil
}

func newControlMessage(opt *rawOpt) (oob []byte) {
	opt.lock()
	defer opt.unlock()
	if opt.isset(FlagTTL) {
		b := make([]byte, syscall.CmsgSpace(1))
		cmsg := (*syscall.Cmsghdr)(unsafe.Pointer(&b[0]))
		cmsg.Level = syscall.IPPROTO_IP
		cmsg.Type = syscall.IP_RECVTTL
		cmsg.SetLen(syscall.CmsgLen(1))
		oob = append(oob, b...)
	}
	if opt.isset(pktinfo) {
		b := make([]byte, syscall.CmsgSpace(syscall.SizeofInet4Pktinfo))
		cmsg := (*syscall.Cmsghdr)(unsafe.Pointer(&b[0]))
		cmsg.Level = syscall.IPPROTO_IP
		cmsg.Type = syscall.IP_PKTINFO
		cmsg.SetLen(syscall.CmsgLen(syscall.SizeofInet4Pktinfo))
		oob = append(oob, b...)
	}
	return
}

func parseControlMessage(b []byte) (*ControlMessage, error) {
	cmsgs, err := syscall.ParseSocketControlMessage(b)
	if err != nil {
		return nil, os.NewSyscallError("parse socket control message", err)
	}
	if len(b) == 0 {
		return nil, nil
	}
	cm := &ControlMessage{}
	for _, m := range cmsgs {
		if m.Header.Level != syscall.IPPROTO_IP {
			continue
		}
		switch m.Header.Type {
		case syscall.IP_TTL:
			cm.TTL = int(*(*byte)(unsafe.Pointer(&m.Data[:1][0])))
		case syscall.IP_PKTINFO:
			pi := (*syscall.Inet4Pktinfo)(unsafe.Pointer(&m.Data[0]))
			cm.IfIndex = int(pi.Ifindex)
			cm.Dst = net.IPv4(pi.Addr[0], pi.Addr[1], pi.Addr[2], pi.Addr[3])
		}
	}
	return cm, nil
}

func marshalControlMessage(cm *ControlMessage) (oob []byte) {
	if cm == nil {
		return
	}
	pi := &syscall.Inet4Pktinfo{}
	pion := false
	if ip := cm.Src.To4(); ip != nil {
		copy(pi.Spec_dst[:], ip[:net.IPv4len])
		pion = true
	}
	if cm.IfIndex != 0 {
		pi.Ifindex = int32(cm.IfIndex)
		pion = true
	}
	if pion {
		b := make([]byte, syscall.CmsgSpace(syscall.SizeofInet4Pktinfo))
		cmsg := (*syscall.Cmsghdr)(unsafe.Pointer(&b[0]))
		cmsg.Level = syscall.IPPROTO_IP
		cmsg.Type = syscall.IP_PKTINFO
		cmsg.SetLen(syscall.CmsgLen(syscall.SizeofInet4Pktinfo))
		data := b[syscall.CmsgLen(0):]
		copy(data[:syscall.SizeofInet4Pktinfo], (*[syscall.SizeofInet4Pktinfo]byte)(unsafe.Pointer(pi))[:syscall.SizeofInet4Pktinfo])
		oob = append(oob, b...)
	}
	return
}
