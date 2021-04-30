// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build aix || linux || netbsd
// +build aix linux netbsd

package socket

import (
	"net"
	"sync"
)

type mmsghdrs []mmsghdr

func (hs mmsghdrs) unpack(ms []Message, parseFn func([]byte, string) (net.Addr, error), hint string) error {
	for i := range hs {
		ms[i].N = int(hs[i].Len)
		ms[i].NN = hs[i].Hdr.controllen()
		ms[i].Flags = hs[i].Hdr.flags()
		if parseFn != nil {
			var err error
			ms[i].Addr, err = parseFn(hs[i].Hdr.name(), hint)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// mmsghdrsPacker packs Message-slices into mmsghdrs (re-)using pre-allocated buffers.
type mmsghdrsPacker struct {
	// hs are the pre-allocated mmsghdrs.
	hs mmsghdrs
	// sockaddrs is the pre-allocated buffer for the Hdr.Name buffers.
	// We use one large buffer for all messages and slice it up.
	sockaddrs []byte
	// vs are the pre-allocated iovecs.
	// We allocate one large buffer for all messages and slice it up. This allows to reuse the buffer
	// if the number of buffers per message is distributed differently between calls.
	vs []iovec
}

func (p *mmsghdrsPacker) prepare(ms []Message) {
	n := len(ms)
	if len(p.hs) < n {
		p.hs = make(mmsghdrs, n)
	}
	if len(p.sockaddrs) < n*sizeofSockaddrInet6 {
		p.sockaddrs = make([]byte, n*sizeofSockaddrInet6)
	}

	nb := 0
	for _, m := range ms {
		nb += len(m.Buffers)
	}
	if len(p.vs) < nb {
		p.vs = make([]iovec, nb)
	}
}

func (p *mmsghdrsPacker) pack(ms []Message, parseFn func([]byte, string) (net.Addr, error), marshalFn func(net.Addr, []byte) int) mmsghdrs {
	hs := p.hs[:len(ms)]
	vsRest := p.vs
	saRest := p.sockaddrs
	for i := range hs {
		nvs := len(ms[i].Buffers)
		vs := vsRest[:nvs]
		vsRest = vsRest[nvs:]

		var sa []byte
		if parseFn != nil {
			sa = saRest[:sizeofSockaddrInet6]
			saRest = saRest[sizeofSockaddrInet6:]
		} else if marshalFn != nil {
			n := marshalFn(ms[i].Addr, saRest)
			sa = saRest[:n]
			saRest = saRest[n:]
		}
		hs[i].Hdr.pack(vs, ms[i].Buffers, ms[i].OOB, sa)
	}
	return hs
}

var defaultMmsghdrsPool = mmsghdrsPool{
	p: sync.Pool{
		New: func() interface{} {
			return new(mmsghdrsPacker)
		},
	},
}

type mmsghdrsPool struct {
	p sync.Pool
}

func (p *mmsghdrsPool) Get(ms []Message) *mmsghdrsPacker {
	packer, _ := p.p.Get().(*mmsghdrsPacker)
	packer.prepare(ms)
	return packer
}

func (p *mmsghdrsPool) Put(packer *mmsghdrsPacker) {
	p.p.Put(packer)
}
