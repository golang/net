// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package webdav etc etc TODO.
package webdav // import "golang.org/x/net/webdav"

// TODO: ETag, properties.

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// TODO: define the PropSystem interface.
type PropSystem interface{}

type Handler struct {
	// FileSystem is the virtual file system.
	FileSystem FileSystem
	// LockSystem is the lock management system.
	LockSystem LockSystem
	// PropSystem is an optional property management system. If non-nil, TODO.
	PropSystem PropSystem
	// Logger is an optional error logger. If non-nil, it will be called
	// for all HTTP requests.
	Logger func(*http.Request, error)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	status, err := http.StatusBadRequest, error(nil)
	if h.FileSystem == nil {
		status, err = http.StatusInternalServerError, errNoFileSystem
	} else if h.LockSystem == nil {
		status, err = http.StatusInternalServerError, errNoLockSystem
	} else {
		// TODO: PROPFIND, PROPPATCH methods.
		switch r.Method {
		case "OPTIONS":
			status, err = h.handleOptions(w, r)
		case "GET", "HEAD", "POST":
			status, err = h.handleGetHeadPost(w, r)
		case "DELETE":
			status, err = h.handleDelete(w, r)
		case "PUT":
			status, err = h.handlePut(w, r)
		case "MKCOL":
			status, err = h.handleMkcol(w, r)
		case "COPY", "MOVE":
			status, err = h.handleCopyMove(w, r)
		case "LOCK":
			status, err = h.handleLock(w, r)
		case "UNLOCK":
			status, err = h.handleUnlock(w, r)
		}
	}

	if status != 0 {
		w.WriteHeader(status)
		if status != http.StatusNoContent {
			w.Write([]byte(StatusText(status)))
		}
	}
	if h.Logger != nil {
		h.Logger(r, err)
	}
}

type nopReleaser struct{}

func (nopReleaser) Release() {}

func (h *Handler) confirmLocks(r *http.Request) (releaser Releaser, status int, err error) {
	hdr := r.Header.Get("If")
	if hdr == "" {
		return nopReleaser{}, 0, nil
	}
	ih, ok := parseIfHeader(hdr)
	if !ok {
		return nil, http.StatusBadRequest, errInvalidIfHeader
	}
	// ih is a disjunction (OR) of ifLists, so any ifList will do.
	for _, l := range ih.lists {
		path := l.resourceTag
		if path == "" {
			path = r.URL.Path
		}
		releaser, err = h.LockSystem.Confirm(time.Now(), path, l.conditions...)
		if err == ErrConfirmationFailed {
			continue
		}
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		return releaser, 0, nil
	}
	return nil, http.StatusPreconditionFailed, ErrLocked
}

func (h *Handler) handleOptions(w http.ResponseWriter, r *http.Request) (status int, err error) {
	allow := "OPTIONS, LOCK, PUT, MKCOL"
	if fi, err := h.FileSystem.Stat(r.URL.Path); err == nil {
		if fi.IsDir() {
			allow = "OPTIONS, LOCK, GET, HEAD, POST, DELETE, TRACE, PROPPATCH, COPY, MOVE, UNLOCK, PUT, PROPFIND"
		} else {
			allow = "OPTIONS, LOCK, GET, HEAD, POST, DELETE, TRACE, PROPPATCH, COPY, MOVE, UNLOCK"
		}
	}

	// http://www.webdav.org/specs/rfc4918.html#dav.compliance.classes
	w.Header().Set("DAV", "1, 2")
	// http://msdn.microsoft.com/en-au/library/cc250217.aspx
	w.Header().Set("MS-Author-Via", "DAV")
	w.Header().Set("Allow", allow)
	return 0, nil
}

func (h *Handler) handleGetHeadPost(w http.ResponseWriter, r *http.Request) (status int, err error) {
	// TODO: check locks for read-only access??
	f, err := h.FileSystem.OpenFile(r.URL.Path, os.O_RDONLY, 0)
	if err != nil {
		return http.StatusNotFound, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return http.StatusNotFound, err
	}
	http.ServeContent(w, r, r.URL.Path, fi.ModTime(), f)
	return 0, nil
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) (status int, err error) {
	releaser, status, err := h.confirmLocks(r)
	if err != nil {
		return status, err
	}
	defer releaser.Release()

	// TODO: return MultiStatus where appropriate.

	// "godoc os RemoveAll" says that "If the path does not exist, RemoveAll
	// returns nil (no error)." WebDAV semantics are that it should return a
	// "404 Not Found". We therefore have to Stat before we RemoveAll.
	if _, err := h.FileSystem.Stat(r.URL.Path); err != nil {
		if os.IsNotExist(err) {
			return http.StatusNotFound, err
		}
		return http.StatusMethodNotAllowed, err
	}
	if err := h.FileSystem.RemoveAll(r.URL.Path); err != nil {
		return http.StatusMethodNotAllowed, err
	}
	return http.StatusNoContent, nil
}

func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request) (status int, err error) {
	releaser, status, err := h.confirmLocks(r)
	if err != nil {
		return status, err
	}
	defer releaser.Release()

	f, err := h.FileSystem.OpenFile(r.URL.Path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return http.StatusNotFound, err
	}
	defer f.Close()
	if _, err := io.Copy(f, r.Body); err != nil {
		return http.StatusMethodNotAllowed, err
	}
	return http.StatusCreated, nil
}

func (h *Handler) handleMkcol(w http.ResponseWriter, r *http.Request) (status int, err error) {
	releaser, status, err := h.confirmLocks(r)
	if err != nil {
		return status, err
	}
	defer releaser.Release()

	if r.ContentLength > 0 {
		return http.StatusUnsupportedMediaType, nil
	}
	if err := h.FileSystem.Mkdir(r.URL.Path, 0777); err != nil {
		if os.IsNotExist(err) {
			return http.StatusConflict, err
		}
		return http.StatusMethodNotAllowed, err
	}
	return http.StatusCreated, nil
}

func (h *Handler) handleCopyMove(w http.ResponseWriter, r *http.Request) (status int, err error) {
	// TODO: COPY/MOVE for Properties, as per sections 9.8.2 and 9.9.1.

	hdr := r.Header.Get("Destination")
	if hdr == "" {
		return http.StatusBadRequest, errInvalidDestination
	}
	u, err := url.Parse(hdr)
	if err != nil {
		return http.StatusBadRequest, errInvalidDestination
	}
	if u.Host != r.Host {
		return http.StatusBadGateway, errInvalidDestination
	}
	// TODO: do we need a webdav.StripPrefix HTTP handler that's like the
	// standard library's http.StripPrefix handler, but also strips the
	// prefix in the Destination header?

	dst, src := u.Path, r.URL.Path
	if dst == src {
		return http.StatusForbidden, errDestinationEqualsSource
	}

	// TODO: confirmLocks should also check dst.
	releaser, status, err := h.confirmLocks(r)
	if err != nil {
		return status, err
	}
	defer releaser.Release()

	if r.Method == "COPY" {
		// Section 9.8.3 says that "The COPY method on a collection without a Depth
		// header must act as if a Depth header with value "infinity" was included".
		depth := infiniteDepth
		if hdr := r.Header.Get("Depth"); hdr != "" {
			depth = parseDepth(hdr)
			if depth != 0 && depth != infiniteDepth {
				// Section 9.8.3 says that "A client may submit a Depth header on a
				// COPY on a collection with a value of "0" or "infinity"."
				return http.StatusBadRequest, errInvalidDepth
			}
		}
		return copyFiles(h.FileSystem, src, dst, r.Header.Get("Overwrite") != "F", depth, 0)
	}

	// Section 9.9.2 says that "The MOVE method on a collection must act as if
	// a "Depth: infinity" header was used on it. A client must not submit a
	// Depth header on a MOVE on a collection with any value but "infinity"."
	if hdr := r.Header.Get("Depth"); hdr != "" {
		if parseDepth(hdr) != infiniteDepth {
			return http.StatusBadRequest, errInvalidDepth
		}
	}

	created := false
	if _, err := h.FileSystem.Stat(dst); err != nil {
		if !os.IsNotExist(err) {
			return http.StatusForbidden, err
		}
		created = true
	} else {
		switch r.Header.Get("Overwrite") {
		case "T":
			// Section 9.9.3 says that "If a resource exists at the destination
			// and the Overwrite header is "T", then prior to performing the move,
			// the server must perform a DELETE with "Depth: infinity" on the
			// destination resource.
			if err := h.FileSystem.RemoveAll(dst); err != nil {
				return http.StatusForbidden, err
			}
		case "F":
			return http.StatusPreconditionFailed, os.ErrExist
		default:
			return http.StatusBadRequest, errInvalidOverwrite
		}
	}
	if err := h.FileSystem.Rename(src, dst); err != nil {
		return http.StatusForbidden, err
	}
	if created {
		return http.StatusCreated, nil
	}
	return http.StatusNoContent, nil
}

func (h *Handler) handleLock(w http.ResponseWriter, r *http.Request) (retStatus int, retErr error) {
	duration, err := parseTimeout(r.Header.Get("Timeout"))
	if err != nil {
		return http.StatusBadRequest, err
	}
	li, status, err := readLockInfo(r.Body)
	if err != nil {
		return status, err
	}

	token, ld, now := "", LockDetails{}, time.Now()
	if li == (lockInfo{}) {
		// An empty lockInfo means to refresh the lock.
		ih, ok := parseIfHeader(r.Header.Get("If"))
		if !ok {
			return http.StatusBadRequest, errInvalidIfHeader
		}
		if len(ih.lists) == 1 && len(ih.lists[0].conditions) == 1 {
			token = ih.lists[0].conditions[0].Token
		}
		if token == "" {
			return http.StatusBadRequest, errInvalidLockToken
		}
		ld, err = h.LockSystem.Refresh(now, token, duration)
		if err != nil {
			if err == ErrNoSuchLock {
				return http.StatusPreconditionFailed, err
			}
			return http.StatusInternalServerError, err
		}

	} else {
		// Section 9.10.3 says that "If no Depth header is submitted on a LOCK request,
		// then the request MUST act as if a "Depth:infinity" had been submitted."
		depth := infiniteDepth
		if hdr := r.Header.Get("Depth"); hdr != "" {
			depth = parseDepth(hdr)
			if depth != 0 && depth != infiniteDepth {
				// Section 9.10.3 says that "Values other than 0 or infinity must not be
				// used with the Depth header on a LOCK method".
				return http.StatusBadRequest, errInvalidDepth
			}
		}
		ld = LockDetails{
			Root:      r.URL.Path,
			Duration:  duration,
			OwnerXML:  li.Owner.InnerXML,
			ZeroDepth: depth == 0,
		}
		token, err = h.LockSystem.Create(now, ld)
		if err != nil {
			if err == ErrLocked {
				return StatusLocked, err
			}
			return http.StatusInternalServerError, err
		}
		defer func() {
			if retErr != nil {
				h.LockSystem.Unlock(now, token)
			}
		}()

		// Create the resource if it didn't previously exist.
		if _, err := h.FileSystem.Stat(r.URL.Path); err != nil {
			f, err := h.FileSystem.OpenFile(r.URL.Path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
			if err != nil {
				// TODO: detect missing intermediate dirs and return http.StatusConflict?
				return http.StatusInternalServerError, err
			}
			f.Close()
			w.WriteHeader(http.StatusCreated)
			// http://www.webdav.org/specs/rfc4918.html#HEADER_Lock-Token says that the
			// Lock-Token value is a Coded-URL. We add angle brackets.
			w.Header().Set("Lock-Token", "<"+token+">")
		}
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	writeLockInfo(w, token, ld)
	return 0, nil
}

func (h *Handler) handleUnlock(w http.ResponseWriter, r *http.Request) (status int, err error) {
	// http://www.webdav.org/specs/rfc4918.html#HEADER_Lock-Token says that the
	// Lock-Token value is a Coded-URL. We strip its angle brackets.
	t := r.Header.Get("Lock-Token")
	if len(t) < 2 || t[0] != '<' || t[len(t)-1] != '>' {
		return http.StatusBadRequest, errInvalidLockToken
	}
	t = t[1 : len(t)-1]

	switch err = h.LockSystem.Unlock(time.Now(), t); err {
	case nil:
		return http.StatusNoContent, err
	case ErrForbidden:
		return http.StatusForbidden, err
	case ErrLocked:
		return StatusLocked, err
	case ErrNoSuchLock:
		return http.StatusConflict, err
	default:
		return http.StatusInternalServerError, err
	}
}

const (
	infiniteDepth = -1
	invalidDepth  = -2
)

// parseDepth maps the strings "0", "1" and "infinity" to 0, 1 and
// infiniteDepth. Parsing any other string returns invalidDepth.
//
// Different WebDAV methods have further constraints on valid depths:
//	- PROPFIND has no further restrictions, as per section 9.1.
//	- COPY accepts only "0" or "infinity", as per section 9.8.3.
//	- MOVE accepts only "infinity", as per section 9.9.2.
//	- LOCK accepts only "0" or "infinity", as per section 9.10.3.
// These constraints are enforced by the handleXxx methods.
func parseDepth(s string) int {
	switch s {
	case "0":
		return 0
	case "1":
		return 1
	case "infinity":
		return infiniteDepth
	}
	return invalidDepth
}

// http://www.webdav.org/specs/rfc4918.html#status.code.extensions.to.http11
const (
	StatusMulti               = 207
	StatusUnprocessableEntity = 422
	StatusLocked              = 423
	StatusFailedDependency    = 424
	StatusInsufficientStorage = 507
)

func StatusText(code int) string {
	switch code {
	case StatusMulti:
		return "Multi-Status"
	case StatusUnprocessableEntity:
		return "Unprocessable Entity"
	case StatusLocked:
		return "Locked"
	case StatusFailedDependency:
		return "Failed Dependency"
	case StatusInsufficientStorage:
		return "Insufficient Storage"
	}
	return http.StatusText(code)
}

var (
	errDestinationEqualsSource = errors.New("webdav: destination equals source")
	errDirectoryNotEmpty       = errors.New("webdav: directory not empty")
	errInvalidDepth            = errors.New("webdav: invalid depth")
	errInvalidDestination      = errors.New("webdav: invalid destination")
	errInvalidIfHeader         = errors.New("webdav: invalid If header")
	errInvalidLockInfo         = errors.New("webdav: invalid lock info")
	errInvalidLockToken        = errors.New("webdav: invalid lock token")
	errInvalidOverwrite        = errors.New("webdav: invalid overwrite")
	errInvalidPropfind         = errors.New("webdav: invalid propfind")
	errInvalidResponse         = errors.New("webdav: invalid response")
	errInvalidTimeout          = errors.New("webdav: invalid timeout")
	errNoFileSystem            = errors.New("webdav: no file system")
	errNoLockSystem            = errors.New("webdav: no lock system")
	errNotADirectory           = errors.New("webdav: not a directory")
	errRecursionTooDeep        = errors.New("webdav: recursion too deep")
	errUnsupportedLockInfo     = errors.New("webdav: unsupported lock info")
)
