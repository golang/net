// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build darwin freebsd netbsd openbsd

package ipv4

import (
	"net"
	"os"
	"syscall"
	"unsafe"
)

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
	if cf&FlagDst != 0 {
		if err := setIPv4ReceiveDestinationAddress(fd, on); err != nil {
			return err
		}
		if on {
			opt.set(FlagDst)
		} else {
			opt.clear(FlagDst)
		}
	}
	if cf&FlagInterface != 0 {
		if err := setIPv4ReceiveInterface(fd, on); err != nil {
			return err
		}
		if on {
			opt.set(FlagInterface)
		} else {
			opt.clear(FlagInterface)
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
	if opt.isset(FlagDst) {
		b := make([]byte, syscall.CmsgSpace(net.IPv4len))
		cmsg := (*syscall.Cmsghdr)(unsafe.Pointer(&b[0]))
		cmsg.Level = syscall.IPPROTO_IP
		cmsg.Type = syscall.IP_RECVDSTADDR
		cmsg.SetLen(syscall.CmsgLen(net.IPv4len))
		oob = append(oob, b...)
	}
	if opt.isset(FlagInterface) {
		b := make([]byte, syscall.CmsgSpace(syscall.SizeofSockaddrDatalink))
		cmsg := (*syscall.Cmsghdr)(unsafe.Pointer(&b[0]))
		cmsg.Level = syscall.IPPROTO_IP
		cmsg.Type = syscall.IP_RECVIF
		cmsg.SetLen(syscall.CmsgLen(syscall.SizeofSockaddrDatalink))
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
		case syscall.IP_RECVTTL:
			cm.TTL = int(*(*byte)(unsafe.Pointer(&m.Data[:1][0])))
		case syscall.IP_RECVDSTADDR:
			v := m.Data[:4]
			cm.Dst = net.IPv4(v[0], v[1], v[2], v[3])
		case syscall.IP_RECVIF:
			sadl := (*syscall.SockaddrDatalink)(unsafe.Pointer(&m.Data[0]))
			cm.IfIndex = int(sadl.Index)
		}
	}
	return cm, nil
}

func marshalControlMessage(cm *ControlMessage) []byte {
	// TODO(mikio): Implement IP_PKTINFO stuff when OS X 10.8 comes
	return nil
}
