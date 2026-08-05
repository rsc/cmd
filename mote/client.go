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
		server, args = args[0][1:], args[1:]
		if len(args) == 0 {
			usage()
		}
	}
	// Resolve the command to the file it names before anything else
	// looks at it: the name travels to the server in Args, which is how
	// the server knows which uploaded file to run.
	args[0] = cmdFile(args[0])
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
	conn, err := dialServer(url)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	w, err := conn.Run(&Exec{
		Args:   args,
		Dir:    filepath.ToSlash(dir),
		Files:  files,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		log.Fatal(conn.abort(err))
	}
	if conn.GOOS != "" && conn.GOARCH != "" {
		name := conn.GOOS + "-" + conn.GOARCH
		if url2, err := lookupAlias(name); err == nil && url2 == "" {
			setAlias(name, url)
		}
	}
	if w.Code < 0 {
		log.Fatalf("remote command killed: %s", w.Status)
	}
	conn.Close()
	os.Exit(w.Code)
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
			goos, goarch, _ := strings.Cut(name, "-")
			builder, err := gomoteBuilder(goos, goarch)
			if err != nil {
				return "", err
			}
			return "gomote://" + builder, nil
		}
	}
	return "", fmt.Errorf("no alias for %s", name)
}

// An Exec describes a command to run on a server.
type Exec struct {
	Args   []string  // command arguments; Args[0] is the command itself
	Dir    string    // client working directory, in slash form
	Files  []*File   // files to place on the server
	Env    []string  // extra environment variables
	Stdout io.Writer // destination for standard output
	Stderr io.Writer // destination for standard error
}

// A Wait describes how a command finished.
type Wait struct {
	Code   int    // exit code (negative if killed by a signal)
	Status string // os.ProcessState description of the exit
}

// Run runs the command described by e on the server at the
// other end of c: setup, upload, start, output streaming, exit status.
func (c *Conn) Run(e *Exec) (*Wait, error) {
	req := &Request{
		Type:  "Setup",
		Files: e.Files,
		Args:  e.Args,
		Dir:   e.Dir,
		Env:   e.Env,
	}
	if err := c.writePacket(req, nil); err != nil {
		return nil, err
	}

	// Answer Need with the upload, until the server says Ready.
	byHash := make(map[string]*File)
	for _, f := range e.Files {
		byHash[f.Hash] = f
	}
Setup:
	for {
		resp, _, err := c.readResponse()
		if err != nil {
			return nil, err
		}
		switch resp.Type {
		default:
			return nil, fmt.Errorf("unexpected response type %q", resp.Type)

		case "Need":
			var readers []io.Reader
			var size int64
			for _, hash := range resp.Need {
				f := byHash[hash]
				if f == nil {
					return nil, fmt.Errorf("server needs unknown hash %s", hash)
				}
				readers = append(readers, io.LimitReader(&lazyFile{name: filepath.FromSlash(f.Path)}, f.Size))
				size += f.Size
			}
			if err := c.writePacketStream(&Request{Type: "Upload"}, size, io.MultiReader(readers...)); err != nil {
				return nil, fmt.Errorf("upload: %v", err)
			}

		case "Ready":
			break Setup
		}
	}

	// The command is about to run; forward interrupts as Kill requests.
	// The connection's write lock keeps a Kill from interleaving with
	// the Start packet.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		<-sig
		c.writePacket(&Request{Type: "Kill"}, nil)
		<-sig
		os.Exit(1)
	}()

	if err := c.writePacket(&Request{Type: "Start"}, nil); err != nil {
		return nil, err
	}
	for {
		resp, data, err := c.readResponse()
		if err != nil {
			return nil, err
		}
		switch resp.Type {
		default:
			return nil, fmt.Errorf("unexpected response type %q", resp.Type)

		case "Output":
			w := e.Stdout
			if resp.Stderr {
				w = e.Stderr
			}
			w.Write(data)

		case "Exit":
			return &Wait{Code: resp.ExitCode, Status: resp.Status}, nil
		}
	}
}

// readResponse reads one response packet,
// turning a Response with Error set into an error.
func (c *Conn) readResponse() (*Response, []byte, error) {
	var resp Response
	data, err := c.readPacket(&resp)
	if err != nil {
		return nil, nil, fmt.Errorf("reading response: %v", err)
	}
	if resp.Error != "" {
		return nil, nil, fmt.Errorf("server: %s", resp.Error)
	}
	return &resp, data, nil
}
