// Copyright 2014 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Gofixerr fixes errors in Go programs.
//
// Usage:
//
//	gofixerr [-v] [-w] [file.go ... | package ...]
//
// Gofixerr attempts to build the package or packages named on the
// command line and then prints suggested changes to fix any recognized
// compiler errors.
//
// The -v flag prints extra output.
//
// The -w flag causes gofixerr to write the changes to the files.
//
// This is an experiment and subject to change.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"rsc.io/rf/diff"
)

var (
	writeFiles = flag.Bool("w", false, "write changes instead of printing diffs")
	verbose    = flag.Bool("v", false, "print verbose output")
)

func usage() {
	fmt.Fprintf(os.Stderr, "usage: gofixerr [-v] [-w] [file.go ...]\n")
	os.Exit(2)
}

var (
	fieldRE        = regexp.MustCompile(`has no field or method ([^ ]+), but does have ([^() ]+)\)`)
	boolCmpRE      = regexp.MustCompile(`cannot use [01] \(type (untyped )?int\) as type bool|[!=]= 0 \(mismatched types untyped bool and untyped int\)`)
	cmpZeroToNilRE = regexp.MustCompile(`invalid operation: .*[!=] 0 \(mismatched types (\*|func|\[\]).* and int\)`)
	useZeroToNilRE = regexp.MustCompile(`cannot use 0 \(type int\) as type (\*|func|\[\]).*`)
)

func main() {
	log.SetPrefix("gofixerr: ")
	log.SetFlags(0)
	flag.Usage = usage
	flag.Parse()

	cmd := exec.Command("go", append([]string{"build", "-gcflags=-e"}, flag.Args()...)...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		log.Fatal("compile succeeded")
	}

	for _, line := range strings.Split(string(out), "\n") {
		i := strings.Index(line, ": ")
		if i < 0 || strings.HasPrefix(line, "#") {
			continue
		}
		file, msg := line[:i], line[i+2:]
		if m := fieldRE.FindStringSubmatch(msg); m != nil {
			b, pos := getbuf(file)
			if pos >= len(b.old) && b.old[pos] != '.' || !bytes.HasPrefix(b.old[pos+1:], []byte(m[1])) {
				log.Printf("%s: out of sync: expected %s", file, m[1])
				continue
			}
			b.Replace(pos+1, pos+1+len(m[1]), m[2])
			continue
		}
		if boolCmpRE.MatchString(msg) {
			b, pos := getbuf(file)
			switch {
			default:
				log.Printf("%s: out of sync: expected '!= 0'", file)

			case bytes.HasPrefix(b.old[pos:], []byte("= 0")):
				b.Replace(pos+2, pos+3, "false")
			case bytes.HasPrefix(b.old[pos:], []byte("= 1")):
				b.Replace(pos+2, pos+3, "true")
			case bytes.HasPrefix(b.old[pos:], []byte("0")):
				b.Replace(pos, pos+1, "false")
			case bytes.HasPrefix(b.old[pos:], []byte("1")):
				b.Replace(pos, pos+1, "true")
			case bytes.HasPrefix(b.old[pos:], []byte("!= 0")):
				b.Delete(pos, pos+len("!= 0"))

			case bytes.HasPrefix(b.old[pos:], []byte("== 0")):
				b.Replace(pos+3, pos+4, "false")
			}
			continue
		}
		if cmpZeroToNilRE.MatchString(msg) {
			b, pos := getbuf(file)
			switch {
			default:
				log.Printf("%s: out of sync: expected '!= 0'", file)

			case bytes.HasPrefix(b.old[pos:], []byte("!= 0")):
				b.Replace(pos+3, pos+4, "nil")

			case bytes.HasPrefix(b.old[pos:], []byte("== 0")):
				b.Replace(pos+3, pos+4, "nil")
			}
			continue
		}
		if useZeroToNilRE.MatchString(msg) {
			b, pos := getbuf(file)
			switch {
			default:
				if i := bytes.Index(b.old[pos:], []byte(" = 0")); 0 <= i && i < 100 && !bytes.Contains(b.old[pos:pos+i], []byte("\n")) {
					// Sometimes positioned at start of declaration.
					b.Replace(pos+i+3, pos+i+4, "nil")
					break
				}
				log.Printf("%s: out of sync: expected 0", file)

			case bytes.HasPrefix(b.old[pos:], []byte("= 0")):
				b.Replace(pos+2, pos+3, "nil")

			case bytes.HasPrefix(b.old[pos:], []byte("0")):
				b.Replace(pos, pos+1, "nil")
			}
		}
	}

	if len(bufs) == 0 {
		log.Fatalf("no fixes")
	}

	var files []string
	for file := range bufs {
		files = append(files, file)
	}
	sort.Strings(files)

	if !*writeFiles {
		for _, file := range files {
			b := bufs[file]
			out := b.Bytes()
			d, err := diff.Diff(file, b.old, file, out)
			if err != nil {
				log.Printf("diff %s: %v", file, err)
				continue
			}
			os.Stdout.Write(d)
		}
		return
	}

	for _, file := range files {
		b := bufs[file]
		out := b.Bytes()
		if err := ioutil.WriteFile(file, out, 0666); err != nil {
			log.Print(err)
		}
	}
}

var bufs = make(map[string]*Buffer)

func getbuf(addr string) (*Buffer, int) {
	i := strings.Index(addr, ":")
	if i < 0 {
		log.Fatalf("bad file address: %s", addr)
	}
	file, lineCol := addr[:i], addr[i+1:]
	b := bufs[file]
	if b == nil {
		data, err := ioutil.ReadFile(file)
		if err != nil {
			log.Fatal(err)
		}
		b = NewBuffer(data)
		bufs[file] = b
	}

	i = strings.Index(lineCol, ":")
	if i < 0 {
		log.Fatalf("bad file address: %s", addr)
	}
	lineStr, colStr := lineCol[:i], lineCol[i+1:]
	line, err := strconv.Atoi(lineStr)
	if err != nil {
		log.Fatalf("bad file address: %s", addr)
	}
	col, err := strconv.Atoi(colStr)
	if err != nil {
		log.Fatalf("bad file address: %s", addr)
	}

	pos := 0
	for ; pos < len(b.old) && line > 1; pos++ {
		if b.old[pos] == '\n' {
			line--
		}
	}
	pos += col - 1
	if pos > len(b.old) {
		pos = len(b.old)
	}
	return b, pos
}
