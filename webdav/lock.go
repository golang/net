// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"errors"
	"io"
	"time"
)

var (
	ErrConfirmationFailed = errors.New("webdav: confirmation failed")
	ErrForbidden          = errors.New("webdav: forbidden")
	ErrNoSuchLock         = errors.New("webdav: no such lock")
)

// Condition can match a WebDAV resource, based on a token or ETag.
// Exactly one of Token and ETag should be non-empty.
type Condition struct {
	Not   bool
	Token string
	ETag  string
}

type LockSystem interface {
	// TODO: comment that the conditions should be ANDed together.
	Confirm(path string, conditions ...Condition) (c io.Closer, err error)
	// TODO: comment that token should be an absolute URI as defined by RFC 3986,
	// Section 4.3. In particular, it should not contain whitespace.
	Create(path string, now time.Time, ld LockDetails) (token string, c io.Closer, err error)
	Refresh(token string, now time.Time, duration time.Duration) (ld LockDetails, c io.Closer, err error)
	Unlock(token string) error
}

type LockDetails struct {
	Depth    int           // Negative means infinite depth.
	Duration time.Duration // Negative means unlimited duration.
	OwnerXML string        // Verbatim XML.
	Path     string
}

// TODO: a MemLS implementation.
