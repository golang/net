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
	"hash"

	"golang.org/x/crypto/chacha20"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/cryptobyte"
	"golang.org/x/crypto/hkdf"
)

var errInvalidPacket = errors.New("quic: invalid packet")

// headerProtectionSampleSize is the size of the ciphertext sample used for header protection.
// https://www.rfc-editor.org/rfc/rfc9001#section-5.4.2
const headerProtectionSampleSize = 16

// aeadOverhead is the difference in size between the AEAD output and input.
// All cipher suites defined for use with QUIC have 16 bytes of overhead.
const aeadOverhead = 16

// A headerKey applies or removes header protection.
// https://www.rfc-editor.org/rfc/rfc9001#section-5.4
type headerKey struct {
	hp headerProtection
}

func (k *headerKey) init(suite uint16, secret []byte) {
	h, keySize := hashForSuite(suite)
	hpKey := hkdfExpandLabel(h.New, secret, "quic hp", nil, keySize)
	switch suite {
	case tls.TLS_AES_128_GCM_SHA256, tls.TLS_AES_256_GCM_SHA384:
		c, err := aes.NewCipher(hpKey)
		if err != nil {
			panic(err)
		}
		k.hp = &aesHeaderProtection{cipher: c}
	case tls.TLS_CHACHA20_POLY1305_SHA256:
		k.hp = chaCha20HeaderProtection{hpKey}
	default:
		panic("BUG: unknown cipher suite")
	}
}

// protect applies header protection.
// pnumOff is the offset of the packet number in the packet.
func (k headerKey) protect(hdr []byte, pnumOff int) {
	// Apply header protection.
	pnumSize := int(hdr[0]&0x03) + 1
	sample := hdr[pnumOff+4:][:headerProtectionSampleSize]
	mask := k.hp.headerProtection(sample)
	if isLongHeader(hdr[0]) {
		hdr[0] ^= mask[0] & 0x0f
	} else {
		hdr[0] ^= mask[0] & 0x1f
	}
	for i := 0; i < pnumSize; i++ {
		hdr[pnumOff+i] ^= mask[1+i]
	}
}

// unprotect removes header protection.
// pnumOff is the offset of the packet number in the packet.
// pnumMax is the largest packet number seen in the number space of this packet.
func (k headerKey) unprotect(pkt []byte, pnumOff int, pnumMax packetNumber) (hdr, pay []byte, pnum packetNumber, _ error) {
	if len(pkt) < pnumOff+4+headerProtectionSampleSize {
		return nil, nil, 0, errInvalidPacket
	}
	numpay := pkt[pnumOff:]
	sample := numpay[4:][:headerProtectionSampleSize]
	mask := k.hp.headerProtection(sample)
	if isLongHeader(pkt[0]) {
		pkt[0] ^= mask[0] & 0x0f
	} else {
		pkt[0] ^= mask[0] & 0x1f
	}
	pnumLen := int(pkt[0]&0x03) + 1
	pnum = packetNumber(0)
	for i := 0; i < pnumLen; i++ {
		numpay[i] ^= mask[1+i]
		pnum = (pnum << 8) | packetNumber(numpay[i])
	}
	pnum = decodePacketNumber(pnumMax, pnum, pnumLen)
	hdr = pkt[:pnumOff+pnumLen]
	pay = numpay[pnumLen:]
	return hdr, pay, pnum, nil
}

// headerProtection is the  header_protection function as defined in:
// https://www.rfc-editor.org/rfc/rfc9001#section-5.4.1
//
// This function takes a sample of the packet ciphertext
// and returns a 5-byte mask which will be applied to the
// protected portions of the packet header.
type headerProtection interface {
	headerProtection(sample []byte) (mask [5]byte)
}

// AES-based header protection.
// https://www.rfc-editor.org/rfc/rfc9001#section-5.4.3
type aesHeaderProtection struct {
	cipher  cipher.Block
	scratch [aes.BlockSize]byte
}

func (hp *aesHeaderProtection) headerProtection(sample []byte) (mask [5]byte) {
	hp.cipher.Encrypt(hp.scratch[:], sample)
	copy(mask[:], hp.scratch[:])
	return mask
}

// ChaCha20-based header protection.
// https://www.rfc-editor.org/rfc/rfc9001#section-5.4.4
type chaCha20HeaderProtection struct {
	key []byte
}

func (hp chaCha20HeaderProtection) headerProtection(sample []byte) (mask [5]byte) {
	counter := uint32(sample[3])<<24 | uint32(sample[2])<<16 | uint32(sample[1])<<8 | uint32(sample[0])
	nonce := sample[4:16]
	c, err := chacha20.NewUnauthenticatedCipher(hp.key, nonce)
	if err != nil {
		panic(err)
	}
	c.SetCounter(counter)
	c.XORKeyStream(mask[:], mask[:])
	return mask
}

// A packetKey applies or removes packet protection.
// https://www.rfc-editor.org/rfc/rfc9001#section-5.1
type packetKey struct {
	aead cipher.AEAD // AEAD function used for packet protection.
	iv   []byte      // IV used to construct the AEAD nonce.
}

func (k *packetKey) init(suite uint16, secret []byte) {
	// https://www.rfc-editor.org/rfc/rfc9001#section-5.1
	h, keySize := hashForSuite(suite)
	key := hkdfExpandLabel(h.New, secret, "quic key", nil, keySize)
	switch suite {
	case tls.TLS_AES_128_GCM_SHA256, tls.TLS_AES_256_GCM_SHA384:
		k.aead = newAESAEAD(key)
	case tls.TLS_CHACHA20_POLY1305_SHA256:
		k.aead = newChaCha20AEAD(key)
	default:
		panic("BUG: unknown cipher suite")
	}
	k.iv = hkdfExpandLabel(h.New, secret, "quic iv", nil, k.aead.NonceSize())
}

func newAESAEAD(key []byte) cipher.AEAD {
	c, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(c)
	if err != nil {
		panic(err)
	}
	return aead
}

func newChaCha20AEAD(key []byte) cipher.AEAD {
	var err error
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		panic(err)
	}
	return aead
}

func (k packetKey) protect(hdr, pay []byte, pnum packetNumber) []byte {
	k.xorIV(pnum)
	defer k.xorIV(pnum)
	return k.aead.Seal(hdr, k.iv, pay, hdr)
}

func (k packetKey) unprotect(hdr, pay []byte, pnum packetNumber) (dec []byte, err error) {
	k.xorIV(pnum)
	defer k.xorIV(pnum)
	return k.aead.Open(pay[:0], k.iv, pay, hdr)
}

// xorIV xors the packet protection IV with the packet number.
func (k packetKey) xorIV(pnum packetNumber) {
	k.iv[len(k.iv)-8] ^= uint8(pnum >> 56)
	k.iv[len(k.iv)-7] ^= uint8(pnum >> 48)
	k.iv[len(k.iv)-6] ^= uint8(pnum >> 40)
	k.iv[len(k.iv)-5] ^= uint8(pnum >> 32)
	k.iv[len(k.iv)-4] ^= uint8(pnum >> 24)
	k.iv[len(k.iv)-3] ^= uint8(pnum >> 16)
	k.iv[len(k.iv)-2] ^= uint8(pnum >> 8)
	k.iv[len(k.iv)-1] ^= uint8(pnum)
}

// A fixedKeys is a header protection key and fixed packet protection key.
// The packet protection key is fixed (it does not update).
//
// Fixed keys are used for Initial and Handshake keys, which do not update.
type fixedKeys struct {
	hdr headerKey
	pkt packetKey
}

func (k *fixedKeys) init(suite uint16, secret []byte) {
	k.hdr.init(suite, secret)
	k.pkt.init(suite, secret)
}

func (k fixedKeys) isSet() bool {
	return k.hdr.hp != nil
}

// protect applies packet protection to a packet.
//
// On input, hdr contains the packet header, pay the unencrypted payload,
// pnumOff the offset of the packet number in the header, and pnum the untruncated
// packet number.
//
// protect returns the result of appending the encrypted payload to hdr and
// applying header protection.
func (k fixedKeys) protect(hdr, pay []byte, pnumOff int, pnum packetNumber) []byte {
	pkt := k.pkt.protect(hdr, pay, pnum)
	k.hdr.protect(pkt, pnumOff)
	return pkt
}

// unprotect removes packet protection from a packet.
//
// On input, pkt contains the full protected packet, pnumOff the offset of
// the packet number in the header, and pnumMax the largest packet number
// seen in the number space of this packet.
//
// unprotect removes header protection from the header in pkt, and returns
// the unprotected payload and packet number.
func (k fixedKeys) unprotect(pkt []byte, pnumOff int, pnumMax packetNumber) (pay []byte, num packetNumber, err error) {
	hdr, pay, pnum, err := k.hdr.unprotect(pkt, pnumOff, pnumMax)
	if err != nil {
		return nil, 0, err
	}
	pay, err = k.pkt.unprotect(hdr, pay, pnum)
	if err != nil {
		return nil, 0, err
	}
	return pay, pnum, nil
}

// A fixedKeyPair is a read/write pair of fixed keys.
type fixedKeyPair struct {
	r, w fixedKeys
}

func (k *fixedKeyPair) discard() {
	*k = fixedKeyPair{}
}

func (k *fixedKeyPair) canRead() bool {
	return k.r.isSet()
}

func (k *fixedKeyPair) canWrite() bool {
	return k.w.isSet()
}

// https://www.rfc-editor.org/rfc/rfc9001#section-5.2-2
var initialSalt = []byte{0x38, 0x76, 0x2c, 0xf7, 0xf5, 0x59, 0x34, 0xb3, 0x4d, 0x17, 0x9a, 0xe6, 0xa4, 0xc8, 0x0c, 0xad, 0xcc, 0xbb, 0x7f, 0x0a}

// initialKeys returns the keys used to protect Initial packets.
//
// The Initial packet keys are derived from the Destination Connection ID
// field in the client's first Initial packet.
//
// https://www.rfc-editor.org/rfc/rfc9001#section-5.2
func initialKeys(cid []byte, side connSide) fixedKeyPair {
	initialSecret := hkdf.Extract(sha256.New, cid, initialSalt)
	var clientKeys fixedKeys
	clientSecret := hkdfExpandLabel(sha256.New, initialSecret, "client in", nil, sha256.Size)
	clientKeys.init(tls.TLS_AES_128_GCM_SHA256, clientSecret)
	var serverKeys fixedKeys
	serverSecret := hkdfExpandLabel(sha256.New, initialSecret, "server in", nil, sha256.Size)
	serverKeys.init(tls.TLS_AES_128_GCM_SHA256, serverSecret)
	if side == clientSide {
		return fixedKeyPair{r: serverKeys, w: clientKeys}
	} else {
		return fixedKeyPair{w: serverKeys, r: clientKeys}
	}
}

// checkCipherSuite returns an error if suite is not a supported cipher suite.
func checkCipherSuite(suite uint16) error {
	switch suite {
	case tls.TLS_AES_128_GCM_SHA256:
	case tls.TLS_AES_256_GCM_SHA384:
	case tls.TLS_CHACHA20_POLY1305_SHA256:
	default:
		return errors.New("invalid cipher suite")
	}
	return nil
}

func hashForSuite(suite uint16) (h crypto.Hash, keySize int) {
	switch suite {
	case tls.TLS_AES_128_GCM_SHA256:
		return crypto.SHA256, 128 / 8
	case tls.TLS_AES_256_GCM_SHA384:
		return crypto.SHA384, 256 / 8
	case tls.TLS_CHACHA20_POLY1305_SHA256:
		return crypto.SHA256, chacha20.KeySize
	default:
		panic("BUG: unknown cipher suite")
	}
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
