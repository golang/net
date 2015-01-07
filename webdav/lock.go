// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"errors"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// ErrConfirmationFailed is returned by a LockSystem's Confirm method.
	ErrConfirmationFailed = errors.New("webdav: confirmation failed")
	// ErrForbidden is returned by a LockSystem's Unlock method.
	ErrForbidden = errors.New("webdav: forbidden")
	// ErrLocked is returned by a LockSystem's Create, Refresh and Unlock methods.
	ErrLocked = errors.New("webdav: locked")
	// ErrNoSuchLock is returned by a LockSystem's Refresh and Unlock methods.
	ErrNoSuchLock = errors.New("webdav: no such lock")
)

// Condition can match a WebDAV resource, based on a token or ETag.
// Exactly one of Token and ETag should be non-empty.
type Condition struct {
	Not   bool
	Token string
	ETag  string
}

// Releaser releases previously confirmed lock claims.
//
// Calling Release does not unlock the lock, in the WebDAV UNLOCK sense, but
// once LockSystem.Confirm has confirmed that a lock claim is valid, that lock
// cannot be Confirmed again until it has been Released.
type Releaser interface {
	Release()
}

// LockSystem manages access to a collection of named resources. The elements
// in a lock name are separated by slash ('/', U+002F) characters, regardless
// of host operating system convention.
type LockSystem interface {
	// Confirm confirms that the caller can claim all of the locks specified by
	// the given conditions, and that holding the union of all of those locks
	// gives exclusive access to the named resource.
	//
	// Exactly one of r and err will be non-nil. If r is non-nil, all of the
	// requested locks are held until r.Release is called.
	//
	// If Confirm returns ErrConfirmationFailed then the Handler will continue
	// to try any other set of locks presented (a WebDAV HTTP request can
	// present more than one set of locks). If it returns any other non-nil
	// error, the Handler will write a "500 Internal Server Error" HTTP status.
	Confirm(now time.Time, name string, conditions ...Condition) (r Releaser, err error)

	// Create creates a lock with the given depth, duration, owner and root
	// (name). The depth will either be negative (meaning infinite) or zero.
	//
	// If Create returns ErrLocked then the Handler will write a "423 Locked"
	// HTTP status. If it returns any other non-nil error, the Handler will
	// write a "500 Internal Server Error" HTTP status.
	//
	// See http://www.webdav.org/specs/rfc4918.html#rfc.section.9.10.6 for
	// when to use each error.
	//
	// The token returned identifies the created lock. It should be an absolute
	// URI as defined by RFC 3986, Section 4.3. In particular, it should not
	// contain whitespace.
	Create(now time.Time, details LockDetails) (token string, err error)

	// Refresh refreshes the lock with the given token.
	//
	// If Refresh returns ErrLocked then the Handler will write a "423 Locked"
	// HTTP Status. If Refresh returns ErrNoSuchLock then the Handler will write
	// a "412 Precondition Failed" HTTP Status. If it returns any other non-nil
	// error, the Handler will write a "500 Internal Server Error" HTTP status.
	//
	// See http://www.webdav.org/specs/rfc4918.html#rfc.section.9.10.6 for
	// when to use each error.
	Refresh(now time.Time, token string, duration time.Duration) (LockDetails, error)

	// Unlock unlocks the lock with the given token.
	//
	// If Unlock returns ErrForbidden then the Handler will write a "403
	// Forbidden" HTTP Status. If Unlock returns ErrLocked then the Handler
	// will write a "423 Locked" HTTP status. If Unlock returns ErrNoSuchLock
	// then the Handler will write a "409 Conflict" HTTP Status. If it returns
	// any other non-nil error, the Handler will write a "500 Internal Server
	// Error" HTTP status.
	//
	// See http://www.webdav.org/specs/rfc4918.html#rfc.section.9.11.1 for
	// when to use each error.
	Unlock(now time.Time, token string) error
}

// LockDetails are a lock's metadata.
type LockDetails struct {
	// Root is the root resource name being locked. For a zero-depth lock, the
	// root is the only resource being locked.
	Root string
	// Depth is the lock depth. A negative depth means infinite.
	//
	// TODO: should depth be restricted to just "0 or infinite" (i.e. change
	// this field to "Recursive bool") or just "0 or 1 or infinite"? Is
	// validating that the responsibility of the Handler or the LockSystem
	// implementations?
	Depth int
	// Duration is the lock timeout. A negative duration means infinite.
	Duration time.Duration
	// OwnerXML is the verbatim <owner> XML given in a LOCK HTTP request.
	//
	// TODO: does the "verbatim" nature play well with XML namespaces?
	// Does the OwnerXML field need to have more structure? See
	// https://codereview.appspot.com/175140043/#msg2
	OwnerXML string
}

// NewMemLS returns a new in-memory LockSystem.
func NewMemLS() LockSystem {
	return &memLS{
		byName:  make(map[string]*memLSNode),
		byToken: make(map[string]*memLSNode),
		gen:     uint64(time.Now().Unix()),
	}
}

type memLS struct {
	mu      sync.Mutex
	byName  map[string]*memLSNode
	byToken map[string]*memLSNode
	gen     uint64
}

func (m *memLS) nextToken() string {
	m.gen++
	return strconv.FormatUint(m.gen, 10)
}

func (m *memLS) collectExpiredNodes(now time.Time) {
	// TODO: implement.
}

func (m *memLS) Confirm(now time.Time, name string, conditions ...Condition) (Releaser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.collectExpiredNodes(now)
	name = path.Clean("/" + name)

	// TODO: touch n.held.
	panic("TODO")
}

func (m *memLS) Create(now time.Time, details LockDetails) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.collectExpiredNodes(now)
	name := path.Clean("/" + details.Root)

	if !m.canCreate(name, details.Depth) {
		return "", ErrLocked
	}
	n := m.create(name)
	n.token = m.nextToken()
	m.byToken[n.token] = n
	n.details = details
	// TODO: set n.expiry.
	return n.token, nil
}

func (m *memLS) Refresh(now time.Time, token string, duration time.Duration) (LockDetails, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.collectExpiredNodes(now)

	n := m.byToken[token]
	if n == nil {
		return LockDetails{}, ErrNoSuchLock
	}
	if n.held {
		return LockDetails{}, ErrLocked
	}
	n.details.Duration = duration
	// TODO: update n.expiry.
	return n.details, nil
}

func (m *memLS) Unlock(now time.Time, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.collectExpiredNodes(now)

	n := m.byToken[token]
	if n == nil {
		return ErrNoSuchLock
	}
	if n.held {
		return ErrLocked
	}
	m.remove(n)
	return nil
}

func (m *memLS) canCreate(name string, depth int) bool {
	return walkToRoot(name, func(name0 string, first bool) bool {
		n := m.byName[name0]
		if n == nil {
			return true
		}
		if first {
			if n.token != "" {
				// The target node is already locked.
				return false
			}
			if depth < 0 {
				// The requested lock depth is infinite, and the fact that n exists
				// (n != nil) means that a descendent of the target node is locked.
				return false
			}
		} else if n.token != "" && n.details.Depth < 0 {
			// An ancestor of the target node is locked with infinite depth.
			return false
		}
		return true
	})
}

func (m *memLS) create(name string) (ret *memLSNode) {
	walkToRoot(name, func(name0 string, first bool) bool {
		n := m.byName[name0]
		if n == nil {
			n = &memLSNode{
				details: LockDetails{
					Root: name0,
				},
			}
			m.byName[name0] = n
		}
		n.refCount++
		if first {
			ret = n
		}
		return true
	})
	return ret
}

func (m *memLS) remove(n *memLSNode) {
	delete(m.byToken, n.token)
	n.token = ""
	walkToRoot(n.details.Root, func(name0 string, first bool) bool {
		x := m.byName[name0]
		x.refCount--
		if x.refCount == 0 {
			delete(m.byName, name0)
		}
		return true
	})
}

func walkToRoot(name string, f func(name0 string, first bool) bool) bool {
	for first := true; ; first = false {
		if !f(name, first) {
			return false
		}
		if name == "/" {
			break
		}
		name = name[:strings.LastIndex(name, "/")]
		if name == "" {
			name = "/"
		}
	}
	return true
}

type memLSNode struct {
	// details are the lock metadata. Even if this node's name is not explicitly locked,
	// details.Root will still equal the node's name.
	details LockDetails
	// token is the unique identifier for this node's lock. An empty token means that
	// this node is not explicitly locked.
	token string
	// refCount is the number of self-or-descendent nodes that are explicitly locked.
	refCount int
	// expiry is when this node's lock expires.
	expiry time.Time
	// held is whether this node's lock is actively held by a Confirm call.
	held bool
}
