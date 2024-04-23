// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Sleep sleeps for a specified duration and then wakes up and exits.
// It is backwards-compatible with the standard Unix sleep(1) command
// but accepts additional duration syntaxes.
//
// Usage:
//
//	sleep <duration>
//	sleep <time>
//
// Duration can be a decimal or hexadecimal floating point number,
// interpreted as a number of aseconds,
// or it can an input accepted by Go's [time.ParseDuration].
//
// Time can be a time accepted by Go's [time.Parse] using one of the following layouts:
//
//	15:04
//	15:04:05
//	3:04pm
//	3:04:05pm
//
// Sleep sleeps until that time. If the time has already occurred today, sleep sleeps
// until that time tomorrow.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"
)

func usage() {
	fmt.Fprintf(os.Stderr, "usage: sleep <duration-or-time>\n")
	os.Exit(2)
}

var formats = []string{
	"15:04",
	"15:04:05",
	"3:04pm",
	"3:04:05pm",
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("sleep: ")
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) != 1 {
		usage()
	}

	if seconds, err := strconv.ParseFloat(args[0], 64); err == nil && seconds > 0 && seconds < (1<<62)/float64(time.Nanosecond) {
		time.Sleep(time.Duration(seconds * float64(time.Second)))
		return
	}

	if d, err := time.ParseDuration(args[0]); err == nil {
		time.Sleep(d)
		return
	}

	for _, f := range formats {
		if t, err := time.Parse(f, args[0]); err == nil {
			now := time.Now()
			when := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
			if when.Before(now) {
				when = when.Add(24 * time.Hour)
			}
			time.Sleep(time.Until(when))
			return
		}
	}

	log.Fatalf("invalid syntax")
}
