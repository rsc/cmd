// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime/debug"
)

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: mote [-u path]... [@name] cmd [args...]\n"+
		"\tmote alias [name [URL]]\n"+
		"\tmote serve URL\n"+
		"\tmote login URL\n"+
		"\tmote go-setup\n"+
		"\tmote version\n")
	flag.PrintDefaults()
	os.Exit(2)
}

var (
	uploads  uploadFlag
	testData = flag.Bool("t", false, "upload testdata directories up to module root")
	verbose  = flag.Bool("v", false, "print verbose output")
)

type uploadFlag []string

func (f *uploadFlag) String() string { return fmt.Sprint([]string(*f)) }

func (f *uploadFlag) Set(s string) error {
	*f = append(*f, s)
	return nil
}

func main() {
	log.SetPrefix("mote: ")
	log.SetFlags(0)

	flag.Var(&uploads, "u", "upload `path` into remote directory tree (may be repeated)")
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
	}

	switch args[0] {
	case "alias":
		cmdAlias(args[1:])
	case "serve":
		cmdServe(args[1:])
	case "login":
		cmdLogin(args[1:])
	case "go-setup":
		cmdGoSetup(args[1:])
	case "version":
		cmdVersion(args[1:])
	default:
		cmdRun(args)
	}
}

func cmdVersion(args []string) {
	if len(args) != 0 {
		usage()
	}
	version := "(unknown)"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}
	fmt.Printf("mote %s\n", version)
}
