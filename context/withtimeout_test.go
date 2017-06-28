// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package context_test

import (
	"fmt"
	"time"

	"golang.org/x/net/context"
)

var workComplete <-chan int

func ExampleWithTimeout() {
	// Pass a context with a timeout to tell a blocking function that it
	// should abandon its work after the timeout elapses.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	select {
	case <-workComplete:
		cancel()
		fmt.Println("overslept")
	case <-ctx.Done():
		fmt.Println(ctx.Err()) // prints "context deadline exceeded"
	}
	// Output:
	// context deadline exceeded
}
