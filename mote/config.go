// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// cmdAlias implements "mote alias [name [URL]]".
func cmdAlias(args []string) {
	switch len(args) {
	case 0:
		aliases, err := readAliases()
		if err != nil {
			log.Fatal(err)
		}
		names := slices.Sorted(maps.Keys(aliases))
		w := 0
		for _, name := range names {
			w = max(w, len(name))
		}
		for _, name := range names {
			fmt.Printf("%-*s %s\n", w, name, aliases[name])
		}
	case 1:
		url, err := lookupAlias(args[0])
		if err != nil {
			log.Fatal(err)
		}
		if url == "" {
			log.Fatalf("no alias for %s", args[0])
		}
		fmt.Printf("%s\n", url)
	case 2:
		if err := setAlias(args[0], args[1]); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
	}
}

func aliasFile() string {
	return filepath.Join(configDir(), "aliases.txt")
}

// readAliases reads the alias definitions from aliases.txt.
func readAliases() (map[string]string, error) {
	aliases := make(map[string]string)
	data, err := os.ReadFile(aliasFile())
	if err != nil {
		if os.IsNotExist(err) {
			return aliases, nil
		}
		return nil, err
	}
	lineno := 0
	for line := range strings.Lines(string(data)) {
		lineno++
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if len(f) != 2 {
			return nil, fmt.Errorf("%s:%d: malformed line: %s", aliasFile(), lineno, strings.TrimSpace(line))
		}
		aliases[f[0]] = f[1]
	}
	return aliases, nil
}

// lookupAlias returns the URL for the named alias,
// or "" if there is no such alias.
func lookupAlias(name string) (string, error) {
	aliases, err := readAliases()
	if err != nil {
		return "", err
	}
	return aliases[name], nil
}

// setAlias adds or replaces the alias definition for name,
// rewriting aliases.txt.
func setAlias(name, url string) error {
	aliases, err := readAliases()
	if err != nil {
		return err
	}
	aliases[name] = url
	var b strings.Builder
	for _, n := range slices.Sorted(maps.Keys(aliases)) {
		fmt.Fprintf(&b, "%s %s\n", n, aliases[n])
	}
	file := aliasFile()
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o666); err != nil {
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
