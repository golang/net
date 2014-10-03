// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipv4

import (
	"syscall"
	"unsafe"
)

func setControlMessage(fd int, opt *rawOpt, cf ControlFlags, on bool) error {
	opt.Lock()
	defer opt.Unlock()
	if cf&FlagTTL != 0 {
		if err := setInt(fd, &sockOpts[ssoReceiveTTL], boolint(on)); err != nil {
			return err
		}
		if on {
			opt.set(FlagTTL)
		} else {
			opt.clear(FlagTTL)
		}
	}
	if cf&(FlagSrc|FlagDst|FlagInterface) != 0 {
		if err := setInt(fd, &sockOpts[ssoPacketInfo], boolint(on)); err != nil {
			return err
		}
		if on {
			opt.set(cf & (FlagSrc | FlagDst | FlagInterface))
		} else {
			opt.clear(cf & (FlagSrc | FlagDst | FlagInterface))
		}
	}
	return nil
}

func (opt *rawOpt) oobLen() (l int) {
	if opt.isset(FlagTTL) {
		l += syscall.CmsgSpace(1)
	}
	if opt.isset(FlagSrc | FlagDst | FlagInterface) {
		l += syscall.CmsgSpace(sysSizeofInetPktinfo)
	}
	return
}

func (opt *rawOpt) marshalControlMessage() (oob []byte) {
	var off int
	oob = make([]byte, opt.oobLen())
	if opt.isset(FlagTTL) {
		m := (*syscall.Cmsghdr)(unsafe.Pointer(&oob[off]))
		m.Level = ianaProtocolIP
		m.Type = sysIP_RECVTTL
		m.SetLen(syscall.CmsgLen(1))
		off += syscall.CmsgSpace(1)
	}
	if opt.isset(FlagSrc | FlagDst | FlagInterface) {
		m := (*syscall.Cmsghdr)(unsafe.Pointer(&oob[0]))
		m.Level = ianaProtocolIP
		m.Type = sysIP_PKTINFO
		m.SetLen(syscall.CmsgLen(sysSizeofInetPktinfo))
		off += syscall.CmsgSpace(sysSizeofInetPktinfo)
	}
	return
}

func (cm *ControlMessage) oobLen() (l int) {
	if cm.Src.To4() != nil || cm.IfIndex != 0 {
		l += syscall.CmsgSpace(sysSizeofInetPktinfo)
	}
	return
}

func (cm *ControlMessage) parseControlMessage(m *syscall.SocketControlMessage) {
	switch m.Header.Type {
	case sysIP_TTL:
		cm.TTL = int(*(*byte)(unsafe.Pointer(&m.Data[:1][0])))
	case sysIP_PKTINFO:
		pi := (*sysInetPktinfo)(unsafe.Pointer(&m.Data[0]))
		cm.IfIndex = int(pi.Ifindex)
		cm.Dst = pi.Addr[:]
	}
}
