// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http3

import "time"

var (
	ShutdownTimeout = 25 * time.Millisecond
)

func init() {
	shutdownTimeout = ShutdownTimeout
}
