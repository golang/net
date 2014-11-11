// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package webdav

import (
	"io"
	"net/http"
	"os"
)

// TODO: comment that paths are always "/"-separated, even for Windows servers.

type FileSystem interface {
	Mkdir(path string, perm os.FileMode) error
	OpenFile(path string, flag int, perm os.FileMode) (File, error)
	RemoveAll(path string) error
	Stat(path string) (os.FileInfo, error)
}

type File interface {
	http.File
	io.Writer
}

// TODO: a MemFS implementation.
// TODO: a RealFS implementation, backed by the real, OS-provided file system.
