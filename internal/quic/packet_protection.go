// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"hash"

	"golang.org/x/crypto/chacha20"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/cryptobyte"
	"golang.org/x/crypto/hkdf"
)

var errInvalidPacket = errors.New("quic: invalid packet")

// keys holds the cryptographic material used to protect packets
// at an encryption level and direction. (e.g., Initial client keys.)
//
// keys are not safe for concurrent use.
type keys struct {
	// AEAD function used for packet protection.
	aead cipher.AEAD

	// The header_protection function as defined in:
	// https://www.rfc-editor.org/rfc/rfc9001#section-5.4.1
	//
	// This function takes a sample of the packet ciphertext
	// and returns a 5-byte mask which will be applied to the
	// protected portions of the packet header.
	headerProtection func(sample []byte) (mask [5]byte)

	// IV used to construct the AEAD nonce.
	iv []byte
}

// newKeys creates keys for a given cipher suite and secret.
//
// It returns an error if the suite is unknown.
func newKeys(suite uint16, secret []byte) (keys, error) {
	switch suite {
	case tls.TLS_AES_128_GCM_SHA256:
		return newAESKeys(secret, crypto.SHA256, 128/8), nil
	case tls.TLS_AES_256_GCM_SHA384:
		return newAESKeys(secret, crypto.SHA384, 256/8), nil
	case tls.TLS_CHACHA20_POLY1305_SHA256:
		return newChaCha20Keys(secret), nil
	}
	return keys{}, fmt.Errorf("unknown cipher suite %x", suite)
}

func newAESKeys(secret []byte, h crypto.Hash, keyBytes int) keys {
	// https://www.rfc-editor.org/rfc/rfc9001#section-5.1
	key := hkdfExpandLabel(h.New, secret, "quic key", nil, keyBytes)
	c, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(c)
	if err != nil {
		panic(err)
	}
	iv := hkdfExpandLabel(h.New, secret, "quic iv", nil, aead.NonceSize())
	// https://www.rfc-editor.org/rfc/rfc9001#section-5.4.3
	hpKey := hkdfExpandLabel(h.New, secret, "quic hp", nil, keyBytes)
	hp, err := aes.NewCipher(hpKey)
	if err != nil {
		panic(err)
	}
	var scratch [aes.BlockSize]byte
	headerProtection := func(sample []byte) (mask [5]byte) {
		hp.Encrypt(scratch[:], sample)
		copy(mask[:], scratch[:])
		return mask
	}
	return keys{
		aead:             aead,
		iv:               iv,
		headerProtection: headerProtection,
	}
}

func newChaCha20Keys(secret []byte) keys {
	// https://www.rfc-editor.org/rfc/rfc9001#section-5.1
	key := hkdfExpandLabel(sha256.New, secret, "quic key", nil, chacha20poly1305.KeySize)
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		panic(err)
	}
	iv := hkdfExpandLabel(sha256.New, secret, "quic iv", nil, aead.NonceSize())
	// https://www.rfc-editor.org/rfc/rfc9001#section-5.4.4
	hpKey := hkdfExpandLabel(sha256.New, secret, "quic hp", nil, chacha20.KeySize)
	headerProtection := func(sample []byte) [5]byte {
		counter := uint32(sample[3])<<24 | uint32(sample[2])<<16 | uint32(sample[1])<<8 | uint32(sample[0])
		nonce := sample[4:16]
		c, err := chacha20.NewUnauthenticatedCipher(hpKey, nonce)
		if err != nil {
			panic(err)
		}
		c.SetCounter(counter)
		var mask [5]byte
		c.XORKeyStream(mask[:], mask[:])
		return mask
	}
	return keys{
		aead:             aead,
		iv:               iv,
		headerProtection: headerProtection,
	}
}

// https://www.rfc-editor.org/rfc/rfc9001#section-5.2-2
var initialSalt = []byte{0x38, 0x76, 0x2c, 0xf7, 0xf5, 0x59, 0x34, 0xb3, 0x4d, 0x17, 0x9a, 0xe6, 0xa4, 0xc8, 0x0c, 0xad, 0xcc, 0xbb, 0x7f, 0x0a}

// initialKeys returns the keys used to protect Initial packets.
//
// The Initial packet keys are derived from the Destination Connection ID
// field in the client's first Initial packet.
//
// https://www.rfc-editor.org/rfc/rfc9001#section-5.2
func initialKeys(cid []byte) (clientKeys, serverKeys keys) {
	initialSecret := hkdf.Extract(sha256.New, cid, initialSalt)
	clientInitialSecret := hkdfExpandLabel(sha256.New, initialSecret, "client in", nil, sha256.Size)
	clientKeys, err := newKeys(tls.TLS_AES_128_GCM_SHA256, clientInitialSecret)
	if err != nil {
		panic(err)
	}

	serverInitialSecret := hkdfExpandLabel(sha256.New, initialSecret, "server in", nil, sha256.Size)
	serverKeys, err = newKeys(tls.TLS_AES_128_GCM_SHA256, serverInitialSecret)
	if err != nil {
		panic(err)
	}

	return clientKeys, serverKeys
}

const headerProtectionSampleSize = 16

// aeadOverhead is the difference in size between the AEAD output and input.
// All cipher suites defined for use with QUIC have 16 bytes of overhead.
const aeadOverhead = 16

// xorIV xors the packet protection IV with the packet number.
func (k keys) xorIV(pnum packetNumber) {
	k.iv[len(k.iv)-8] ^= uint8(pnum >> 56)
	k.iv[len(k.iv)-7] ^= uint8(pnum >> 48)
	k.iv[len(k.iv)-6] ^= uint8(pnum >> 40)
	k.iv[len(k.iv)-5] ^= uint8(pnum >> 32)
	k.iv[len(k.iv)-4] ^= uint8(pnum >> 24)
	k.iv[len(k.iv)-3] ^= uint8(pnum >> 16)
	k.iv[len(k.iv)-2] ^= uint8(pnum >> 8)
	k.iv[len(k.iv)-1] ^= uint8(pnum)
}

// isSet returns true if valid keys are available.
func (k keys) isSet() bool {
	return k.aead != nil
}

// discard discards the keys (in the sense that we won't use them any more,
// not that the keys are securely erased).
//
// https://www.rfc-editor.org/rfc/rfc9001.html#section-4.9
func (k *keys) discard() {
	*k = keys{}
}

// protect applies packet protection to a packet.
//
// On input, hdr contains the packet header, pay the unencrypted payload,
// pnumOff the offset of the packet number in the header, and pnum the untruncated
// packet number.
//
// protect returns the result of appending the encrypted payload to hdr and
// applying header protection.
func (k keys) protect(hdr, pay []byte, pnumOff int, pnum packetNumber) []byte {
	k.xorIV(pnum)
	hdr = k.aead.Seal(hdr, k.iv, pay, hdr)
	k.xorIV(pnum)

	// Apply header protection.
	pnumSize := int(hdr[0]&0x03) + 1
	sample := hdr[pnumOff+4:][:headerProtectionSampleSize]
	mask := k.headerProtection(sample)
	if isLongHeader(hdr[0]) {
		hdr[0] ^= mask[0] & 0x0f
	} else {
		hdr[0] ^= mask[0] & 0x1f
	}
	for i := 0; i < pnumSize; i++ {
		hdr[pnumOff+i] ^= mask[1+i]
	}

	return hdr
}

// unprotect removes packet protection from a packet.
//
// On input, pkt contains the full protected packet, pnumOff the offset of
// the packet number in the header, and pnumMax the largest packet number
// seen in the number space of this packet.
//
// unprotect removes header protection from the header in pkt, and returns
// the unprotected payload and packet number.
func (k keys) unprotect(pkt []byte, pnumOff int, pnumMax packetNumber) (pay []byte, num packetNumber, err error) {
	if len(pkt) < pnumOff+4+headerProtectionSampleSize {
		return nil, 0, errInvalidPacket
	}
	numpay := pkt[pnumOff:]
	sample := numpay[4:][:headerProtectionSampleSize]
	mask := k.headerProtection(sample)
	if isLongHeader(pkt[0]) {
		pkt[0] ^= mask[0] & 0x0f
	} else {
		pkt[0] ^= mask[0] & 0x1f
	}
	pnumLen := int(pkt[0]&0x03) + 1
	pnum := packetNumber(0)
	for i := 0; i < pnumLen; i++ {
		numpay[i] ^= mask[1+i]
		pnum = (pnum << 8) | packetNumber(numpay[i])
	}
	pnum = decodePacketNumber(pnumMax, pnum, pnumLen)

	hdr := pkt[:pnumOff+pnumLen]
	pay = numpay[pnumLen:]
	k.xorIV(pnum)
	pay, err = k.aead.Open(pay[:0], k.iv, pay, hdr)
	k.xorIV(pnum)
	if err != nil {
		return nil, 0, err
	}

	return pay, pnum, nil
}

// hdkfExpandLabel implements HKDF-Expand-Label from RFC 8446, Section 7.1.
//
// Copied from crypto/tls/key_schedule.go.
func hkdfExpandLabel(hash func() hash.Hash, secret []byte, label string, context []byte, length int) []byte {
	var hkdfLabel cryptobyte.Builder
	hkdfLabel.AddUint16(uint16(length))
	hkdfLabel.AddUint8LengthPrefixed(func(b *cryptobyte.Builder) {
		b.AddBytes([]byte("tls13 "))
		b.AddBytes([]byte(label))
	})
	hkdfLabel.AddUint8LengthPrefixed(func(b *cryptobyte.Builder) {
		b.AddBytes(context)
	})
	out := make([]byte, length)
	n, err := hkdf.Expand(hash, secret, hkdfLabel.BytesOrPanic()).Read(out)
	if err != nil || n != length {
		panic("quic: HKDF-Expand-Label invocation failed unexpectedly")
	}
	return out
}
