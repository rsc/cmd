// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

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
// is already in the cache.
func inCache(hash string, size int64) bool {
	info, err := os.Stat(cacheFile(hash))
	return err == nil && info.Size() == size
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
	p = path.Clean("/" + p) // now absolute and .. -free above root
	if strings.Contains(p, `\`) {
		return "", fmt.Errorf("invalid path %#q", slashPath)
	}
	return filepath.Join(tmpdir, filepath.FromSlash(p)), nil
}
