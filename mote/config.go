// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"log"
	"os"
	"path/filepath"
)

// configDir returns the mote configuration directory,
// creating it if necessary.
// $MOTECONFIG overrides the default, for testing.
func configDir() string {
	dir := os.Getenv("MOTECONFIG")
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			log.Fatalf("finding config directory: %v", err)
		}
		dir = filepath.Join(base, "mote")
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		log.Fatal(err)
	}
	return dir
}

// cacheDir returns the mote server's content-addressed cache directory,
// creating it if necessary.
// $MOTECACHE overrides the default, for testing.
func cacheDir() string {
	dir := os.Getenv("MOTECACHE")
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			log.Fatalf("finding cache directory: %v", err)
		}
		dir = filepath.Join(base, "mote", "cache")
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		log.Fatal(err)
	}
	return dir
}
