// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Agentweb lists Claude Code conversations and formats them as HTML.
//
// Usage:
//
//	agentweb [-a] [-o file] [conversation]
//
// With no arguments, agentweb prints the ten most recent conversations it
// finds in $HOME/.claude, one per line, giving each conversation's time,
// ID, and title.
//
// With an argument, agentweb formats that conversation as standalone HTML in
// the file conversation-id.html in the current directory and then opens the
// file in a web browser. The argument can be a conversation ID, a leading
// part of one that belongs to only a single conversation, or any text that
// appears in the title of exactly one conversation.
//
// The -o flag sets where to write the HTML instead, and leaves the browser
// alone, for making a page to keep or to publish rather than one to read
// right now. As in the go command, naming a directory writes the default
// file name into that directory.
//
// By default the formatted conversation shows only what the user and the
// model said to each other. The -a flag adds everything else the transcript
// holds: the model's thinking, and the tool calls it made along the way.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

var (
	all    = flag.Bool("a", false, "show `all` of the conversation, including thinking and tool calls")
	output = flag.String("o", "", "write the conversation to `file` and do not open a browser")
)

func usage() {
	fmt.Fprintf(os.Stderr, "usage: agentweb [-a] [-o file] [conversation]\n")
	flag.PrintDefaults()
	os.Exit(2)
}

// listLimit is how many conversations agentweb lists when run with no arguments.
const listLimit = 10

func main() {
	log.SetPrefix("agentweb: ")
	log.SetFlags(0)
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() > 1 {
		usage()
	}
	if flag.NArg() == 0 && *output != "" {
		log.Fatal("-o needs a conversation to write")
	}

	convs := findConvs()
	if len(convs) == 0 {
		log.Fatal("no conversations found")
	}
	if flag.NArg() == 0 {
		for _, c := range convs[:min(len(convs), listLimit)] {
			fmt.Printf("%s  %s  %s\n", c.Time.Format("2006-01-02 15:04"), c.ID, c.Title)
		}
		return
	}

	c, err := lookup(convs, flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	msgs, err := c.Messages()
	if err != nil {
		log.Fatal(err)
	}
	if !*all {
		msgs = onlySaid(msgs)
	}
	file := outputFile(*output, c)
	if err := os.WriteFile(file, renderHTML(c, msgs), 0666); err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "agentweb: wrote %s\n", file)

	// Writing somewhere named is for keeping the page, not for reading it now.
	if *output != "" {
		return
	}
	if err := openBrowser(file); err != nil {
		log.Fatal(err)
	}
}

// outputFile returns the file to write the formatted conversation to.
// An empty out asks for the default, the conversation ID in the current
// directory. As in the go command's -o, naming a directory that already
// exists writes the default file name inside it.
func outputFile(out string, c *Conv) string {
	name := c.ID + ".html"
	if out == "" {
		return name
	}
	if info, err := os.Stat(out); err == nil && info.IsDir() {
		return filepath.Join(out, name)
	}
	return out
}

// findConvs returns all the conversations agentweb can find,
// most recently active first.
func findConvs() []*Conv {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	convs := claudeConvs(filepath.Join(home, ".claude"))
	slices.SortStableFunc(convs, func(x, y *Conv) int {
		return y.Time.Compare(x.Time)
	})
	return convs
}

// lookup returns the one conversation that arg identifies. An arg that is a
// conversation ID, or the leading part of one, names that conversation.
// Anything else is matched against the conversation titles.
func lookup(convs []*Conv, arg string) (*Conv, error) {
	for _, c := range convs {
		if c.ID == arg {
			return c, nil
		}
	}
	// A leading part of an ID, so long as it belongs to only one
	// conversation. The empty string is not a prefix of anything here;
	// it falls through and matches every title instead.
	if arg != "" {
		var match []*Conv
		for _, c := range convs {
			if strings.HasPrefix(c.ID, arg) {
				match = append(match, c)
			}
		}
		if len(match) > 0 {
			return only(match, arg, "have IDs starting with")
		}
	}
	var match []*Conv
	for _, c := range convs {
		if strings.Contains(strings.ToLower(c.Title), strings.ToLower(arg)) {
			match = append(match, c)
		}
	}
	if len(match) == 0 {
		return nil, fmt.Errorf("no conversation has an ID or title matching %q", arg)
	}
	return only(match, arg, "have titles matching")
}

// only returns the single conversation in match, or an error naming them all.
func only(match []*Conv, arg, why string) (*Conv, error) {
	if len(match) == 1 {
		return match[0], nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d conversations %s %q:", len(match), why, arg)
	for _, c := range match {
		fmt.Fprintf(&b, "\n\t%s  %s", c.ID, c.Title)
	}
	return nil, errors.New(b.String())
}

// openBrowser opens file in the user's web browser.
func openBrowser(file string) error {
	abs, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", abs)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", abs)
	default:
		cmd = exec.Command("xdg-open", abs)
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
