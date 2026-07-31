// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
)

// cmdRun implements the default mote command: run cmd on a server.
func cmdRun(args []string) {
	server := ""
	if strings.HasPrefix(args[0], "@") {
		server = args[0][1:]
		args = args[1:]
		if len(args) == 0 {
			usage()
		}
	}
	files, err := uploadList(args[0], uploads, *testData)
	if err != nil {
		log.Fatal(err)
	}
	url, err := resolveServer(server, args[0], files)
	if err != nil {
		log.Fatal(err)
	}
	dir, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	rwc, password, err := dialServer(url)
	if err != nil {
		log.Fatal(err)
	}
	defer rwc.Close()

	exitCode, status, goos, goarch, err := runSession(rwc, password, files, filepath.ToSlash(dir), args, os.Stdout, os.Stderr)
	if err != nil {
		log.Fatal(err)
	}
	if goos != "" && goarch != "" {
		name := goos + "-" + goarch
		if url2, err := lookupAlias(name); err == nil && url2 == "" {
			setAlias(name, url)
		}
	}
	if exitCode < 0 {
		log.Fatalf("remote command killed: %s", status)
	}
	rwc.Close()
	os.Exit(exitCode)
}

// goosGoarchRE matches a plausible $GOOS-$GOARCH pair like linux-amd64.
var goosGoarchRE = regexp.MustCompile(`^[a-z0-9]+-[a-z0-9]+$`)

// resolveServer resolves the @name argument (possibly empty) to a server URL.
// An empty name falls back to $MOTE, then $GOOS-$GOARCH from the
// environment, then the GOOS-GOARCH of the binary being uploaded.
func resolveServer(name, cmdName string, files []*File) (string, error) {
	if name == "" {
		switch {
		case os.Getenv("MOTE") != "":
			name = os.Getenv("MOTE")
		case os.Getenv("GOOS") != "" && os.Getenv("GOARCH") != "":
			name = os.Getenv("GOOS") + "-" + os.Getenv("GOARCH")
		case isFileCmd(cmdName):
			goos, goarch, err := binaryOSArch(cmdName)
			if err != nil {
				return "", fmt.Errorf("cannot choose server: %v", err)
			}
			name = goos + "-" + goarch
		default:
			return "", fmt.Errorf("no server specified (set $MOTE or use @server)")
		}
	}
	if strings.Contains(name, "://") {
		return name, nil
	}
	url, err := lookupAlias(name)
	if err != nil {
		return "", err
	}
	if url != "" {
		return url, nil
	}
	if goosGoarchRE.MatchString(name) {
		if _, err := exec.LookPath("gomote"); err == nil {
			return "gomote://gotip-" + name, nil
		}
	}
	return "", fmt.Errorf("no alias for %s", name)
}

// runSession runs one client session on rw: handshake, optional
// encryption, Run request, upload, output streaming, exit status.
func runSession(rw io.ReadWriter, password string, files []*File, dir string, args []string, stdout, stderr io.Writer) (exitCode int, status, goos, goarch string, err error) {
	fail := func(err error) (int, string, string, string, error) {
		return 0, "", goos, goarch, err
	}
	if err := clientHandshake(rw); err != nil {
		return fail(err)
	}
	if password != "" {
		s, err := secureClient(rw, password)
		if err != nil {
			return fail(err)
		}
		rw = s
	}
	pc := newPacketConn(rw)
	err = pc.writePacket(&Request{
		Type:  "Run",
		Files: files,
		Cmd:   args[0],
		Args:  args,
		Dir:   dir,
	}, nil)
	if err != nil {
		return fail(err)
	}

	byHash := make(map[string]*File)
	for _, f := range files {
		byHash[f.Hash] = f
	}
	var sig chan os.Signal
	for {
		var resp Response
		data, err := pc.readPacket(&resp)
		if err != nil {
			return fail(fmt.Errorf("reading response: %v", err))
		}
		if resp.GOOS != "" {
			goos, goarch = resp.GOOS, resp.GOARCH
		}
		if resp.Error != "" {
			return fail(fmt.Errorf("server: %s", resp.Error))
		}
		switch resp.Type {
		default:
			return fail(fmt.Errorf("unexpected response type %q", resp.Type))

		case "Need":
			var readers []io.Reader
			var size int64
			for _, hash := range resp.Need {
				f := byHash[hash]
				if f == nil {
					return fail(fmt.Errorf("server needs unknown hash %s", hash))
				}
				readers = append(readers, io.LimitReader(&lazyFile{name: filepath.FromSlash(f.Path)}, f.Size))
				size += f.Size
			}
			if err := pc.writePacketStream(&Request{Type: "Upload"}, size, io.MultiReader(readers...)); err != nil {
				return fail(fmt.Errorf("upload: %v", err))
			}

		case "Start":
			// Command is running; forward interrupts as Kill requests.
			sig = make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt)
			go func() {
				<-sig
				pc.writePacket(&Request{Type: "Kill"}, nil)
				<-sig
				os.Exit(1)
			}()

		case "Output":
			w := stdout
			if resp.Stderr {
				w = stderr
			}
			w.Write(data)

		case "Exit":
			if sig != nil {
				signal.Stop(sig)
			}
			return resp.ExitCode, resp.Status, goos, goarch, nil
		}
	}
}
