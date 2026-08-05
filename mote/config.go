// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"fmt"
	"log"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/term"
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

// cmdLogin implements "mote login URL", establishing the credentials
// that a transport needs before it can be used: Tailscale credentials
// for tail://name, or the password for tcp://host:port.
func cmdLogin(args []string) {
	if len(args) != 1 {
		usage()
	}
	const form = "login URL must have the form tail://name or tcp://host:port"
	u, err := url.Parse(args[0])
	if err != nil {
		log.Fatalf("%s", form)
	}
	switch u.Scheme {
	default:
		log.Fatalf("%s", form)
	case "tail":
		if u.Host == "" {
			log.Fatalf("%s", form)
		}
		if err := tailLogin(u.Host); err != nil {
			log.Fatal(err)
		}
		log.Printf("logged in as mote-%s", u.Host)
	case "tcp":
		if err := checkTCPURL(u); err != nil {
			log.Fatal(err)
		}
		if u.Port() == "" {
			log.Fatalf("%s", form)
		}
		key := tcpKey(u)
		password, err := promptPassword(key)
		if err != nil {
			log.Fatal(err)
		}
		if err := setPassword(key, password); err != nil {
			log.Fatal(err)
		}
		log.Printf("wrote password for %s to %s", key, passwordFile())
	}
}

func passwordFile() string {
	return filepath.Join(configDir(), "password.txt")
}

// promptPassword prompts for the password to share with the server
// named by key, without echoing it when standard input is a terminal.
func promptPassword(key string) (string, error) {
	fmt.Fprintf(os.Stderr, "password for %s: ", key)
	line, err := readLine()
	if err != nil {
		return "", err
	}
	password := strings.TrimSpace(line)
	if password == "" {
		return "", fmt.Errorf("no password provided")
	}
	return password, nil
}

// readLine reads one line from standard input, suppressing the echo
// of a terminal so that a password does not appear on the screen.
func readLine() (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintf(os.Stderr, "\n") // ReadPassword swallowed the typed newline
		return string(line), err
	}
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return "", sc.Err()
	}
	return sc.Text(), nil
}

// readPasswords reads the password definitions from password.txt,
// keyed by server URL.
func readPasswords() (map[string]string, error) {
	passwords := make(map[string]string)
	data, err := os.ReadFile(passwordFile())
	if err != nil {
		if os.IsNotExist(err) {
			return passwords, nil
		}
		return nil, err
	}
	lineno := 0
	for line := range strings.Lines(string(data)) {
		lineno++
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		// Split at the first space or tab instead of using Fields:
		// everything after it is the password, spaces and all.
		// The error mentions no part of the line: whatever is malformed
		// about it, it is a line from the password file.
		i := strings.IndexAny(line, " \t")
		if i < 0 {
			return nil, fmt.Errorf("%s:%d: malformed line", passwordFile(), lineno)
		}
		key, password := line[:i], strings.TrimLeft(line[i:], " \t")
		if password == "" {
			return nil, fmt.Errorf("%s:%d: malformed line", passwordFile(), lineno)
		}
		passwords[key] = password
	}
	return passwords, nil
}

// lookupPassword returns the password saved for the server URL key
// by "mote login".
func lookupPassword(key string) (string, error) {
	passwords, err := readPasswords()
	if err != nil {
		return "", err
	}
	if passwords[key] == "" {
		return "", fmt.Errorf("no password for %s; run mote login %s", key, key)
	}
	return passwords[key], nil
}

// setPassword adds or replaces the password for the server URL key,
// rewriting password.txt.
func setPassword(key, password string) error {
	passwords, err := readPasswords()
	if err != nil {
		return err
	}
	passwords[key] = password
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(passwords)) {
		fmt.Fprintf(&b, "%s %s\n", k, passwords[k])
	}
	file := passwordFile()
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
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
