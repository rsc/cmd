// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Rdate prints the current time in RFC3339 format.
package main

import (
	"fmt"
	"time"
)

func main() {
	fmt.Printf("%s\n", time.Now().Format(time.RFC3339))
}
