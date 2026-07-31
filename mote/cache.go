// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// cacheMaxAge is how long an unused cached file survives cleanCache.
const cacheMaxAge = 3 * time.Hour

// cmdClean implements "mote clean", deleting the entire cache.
func cmdClean(args []string) {
	if len(args) != 0 {
		usage()
	}
	if err := os.RemoveAll(cacheDir()); err != nil {
		log.Fatal(err)
	}
}

// cleanCache deletes cached files that have gone unused for longer
// than cacheMaxAge. (inCache updates the modification time of the
// files it finds, so recently used files are safe.)
func cleanCache() {
	dir := cacheDir()
	cutoff := time.Now().Add(-cacheMaxAge)
	shards, _ := os.ReadDir(dir)
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(dir, shard.Name()))
		for _, f := range files {
			info, err := f.Info()
			if err == nil && info.ModTime().Before(cutoff) {
				os.Remove(filepath.Join(dir, shard.Name(), f.Name()))
			}
		}
	}
}

// validHash reports whether hash is a well-formed lowercase hex SHA-256,
// safe to use as a file name in the cache.
func validHash(hash string) bool {
	if len(hash) != 2*sha256.Size {
		return false
	}
	for i := 0; i < len(hash); i++ {
		c := hash[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// cacheFile returns the name of the cache file for the given hash.
func cacheFile(hash string) string {
	return filepath.Join(cacheDir(), hash[:2], hash)
}

// inCache reports whether the file with the given hash and size
// is already in the cache, marking it recently used if so.
func inCache(hash string, size int64) bool {
	file := cacheFile(hash)
	info, err := os.Stat(file)
	if err != nil || info.Size() != size {
		return false
	}
	now := time.Now()
	os.Chtimes(file, now, now)
	return true
}

// saveToCache reads size bytes from r and saves them in the cache,
// verifying that they have the given hash.
func saveToCache(hash string, size int64, r io.Reader) error {
	file := cacheFile(hash)
	if err := os.MkdirAll(filepath.Dir(file), 0o777); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), "tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, size))
	if err != nil {
		tmp.Close()
		return err
	}
	if n != size {
		tmp.Close()
		return fmt.Errorf("upload of %s: short read: %d bytes, want %d", hash, n, size)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != hash {
		tmp.Close()
		return fmt.Errorf("upload of %s: content has hash %s", hash, sum)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

// copyFromCache copies the cache file with the given hash to dst,
// making it executable. It copies rather than hard-linking so that
// the command cannot corrupt the cache by writing to its files.
func copyFromCache(hash string, dst string) error {
	src, err := os.Open(cacheFile(hash))
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o777)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// remotePath converts the client's absolute slash-separated path
// into a path in the server's temporary directory tree, stripping
// any leading Windows volume name and rejecting paths that would
// escape the tree.
func remotePath(tmpdir, slashPath string) (string, error) {
	p := slashPath
	if len(p) >= 2 && p[1] == ':' { // Windows volume like C:
		p = p[2:]
	}
	if strings.Contains(p, `\`) || slices.Contains(strings.Split(p, "/"), "..") {
		return "", fmt.Errorf("invalid path %#q", slashPath)
	}
	p = path.Clean("/" + p) // now absolute
	return filepath.Join(tmpdir, filepath.FromSlash(p)), nil
}
