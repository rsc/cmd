// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

var usageMessage = `usage:
	review [-a addr] [-n]
	review comments [-json] [-all] [-drafts] [-s n] [-c n] [change]
	review publish [change]
	review reply [-from name] [-resolve] thread [text]
	review resolve thread...
	review restart [-a addr]
	review skill [-print] [-install] [-project]
	review snapshot [-nopin] [change...]
	review snapshots [change]
	review stop
	review unresolve thread...

Flags follow the command they belong to. Every command takes -db file.
`

func usage() {
	fmt.Fprint(os.Stderr, usageMessage)
	os.Exit(2)
}

// defaultAddr is where the background server listens unless told otherwise.
const defaultAddr = "localhost:8781"

// Flags shared by more than one command. They are registered on each
// command's own flag set rather than before the command, so that every
// flag is written after the command it applies to.
var (
	dbFile string
	addr   string
	noOpen bool
)

// serveFlags adds the flags of the commands that run or reach the server.
func serveFlags(f *flag.FlagSet) {
	f.StringVar(&addr, "a", defaultAddr, "serve HTTP requests on `addr`")
	f.BoolVar(&noOpen, "n", false, "do not open a browser")
}

// dbPath is the database this invocation works on.
func dbPath() string {
	if dbFile != "" {
		if abs, err := filepath.Abs(dbFile); err == nil {
			return abs
		}
		return dbFile
	}
	return DefaultDBPath()
}

// stderr and stdout are variables so that tests can capture output.
var (
	stderr io.Writer = os.Stderr
	stdout io.Writer = os.Stdout
)

func main() {
	log.SetPrefix("review: ")
	log.SetFlags(0)

	args := os.Args[1:]
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		// No command: start or join the server and open a browser.
		cmdOpen(args)
		return
	}

	switch args[0] {
	case "comments":
		cmdComments(args[1:])
	case "publish":
		cmdPublish(args[1:])
	case "reply":
		cmdReply(args[1:])
	case "resolve":
		cmdResolve(args[1:], true)
	case "restart":
		cmdRestart(args[1:])
	case "serve":
		// Not in usageMessage: review runs this for itself, in the
		// background. See daemon.go.
		cmdServe(args[1:])
	case "skill":
		cmdSkill(args[1:])
	case "snapshot":
		cmdSnapshot(args[1:])
	case "snapshots":
		cmdSnapshots(args[1:])
	case "stop":
		cmdStop(args[1:])
	case "unresolve":
		cmdResolve(args[1:], false)
	default:
		usage()
	}
}

// open finds the repository containing the current directory and attaches
// the review database to it.
func open(nopin bool) *Review {
	dir, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	repo, err := OpenRepo(dir)
	if err != nil {
		log.Fatal(err)
	}
	db, err := OpenDB(dbPath())
	if err != nil {
		log.Fatal(err)
	}
	return &Review{Repo: repo, DB: db, Pin: !nopin}
}
