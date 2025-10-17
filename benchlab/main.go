// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
)

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: benchlab [options]\n")
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	log.SetPrefix("benchlab: ")
	log.SetFlags(0)

	var chdir string
	var lab Lab
	lab.Init(flag.CommandLine)
	flag.StringVar(&chdir, "C", "", "change to `dir` immediately at startup")
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() != 0 {
		usage()
	}
	if chdir != "" {
		if err := os.Chdir(chdir); err != nil {
			log.Fatal(err)
		}
	}
	if err := lab.Run(); err != nil {
		log.Fatal(err)
	}
	os.Stdout.WriteString(lab.Stats())
}
