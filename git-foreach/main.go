// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Git-foreach runs a command at every Git commit in a sequence.
//
// This command display a line for each commit being tested, along
// with a timer to show progress. When the command finishes, it shows
// whether or not it exited successfully. If the command failed, it
// leaves the command's output in a file named "log.<hash>".
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

// stop is used to coordinate cleaning stopping on a signal.
var stop struct {
	sync.Mutex
	sig  os.Signal
	proc *os.Process
}

// origHEAD is the original value of the HEAD ref.
var origHEAD string

// pretty indicates the terminal supports vt100 control codes.
var pretty bool

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s rev-list cmd...\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() < 2 {
		flag.Usage()
		os.Exit(2)
	}
	revList := flag.Arg(0)
	cmd := flag.Args()[1:]

	// Verify clean working tree.
	if out, ok := tryGit("diff-index", "--quiet", "HEAD", "--"); !ok {
		if len(out) > 0 {
			die("%s", out)
		}
		die("working tree is not clean")
	}

	// Save current HEAD so we can restore it.
	if sym, ok := tryGit("symbolic-ref", "-q", "--short", "HEAD"); ok {
		origHEAD = sym
	} else if sha, ok := tryGit("rev-parse", "--verify", "HEAD"); ok {
		origHEAD = sha
	} else {
		die("bad HEAD")
	}

	// Catch signals to exit loop.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		stop.Lock()
		stop.sig = sig
		if stop.proc != nil {
			//stop.proc.Kill()
			// Kill the process group.
			syscall.Kill(-stop.proc.Pid, sig.(syscall.Signal))
		}
		signal.Stop(sigChan)
		stop.Unlock()
	}()

	pretty = !(os.Getenv("TERM") == "" || os.Getenv("TERM") == "dumb") && term.IsTerminal(syscall.Stdout)

	// Iterate over revisions.
	exitStatus := 0
	for _, rev := range strings.Fields(git("rev-list", "--reverse", revList)) {
		msg := git("rev-list", "-n", "1", "--oneline", rev)
		msg = strings.TrimSpace(msg)
		fmt.Print(msg)

		// Ensure mtimes are updated by checkout.
		for start := time.Now().Unix(); start == time.Now().Unix(); {
			time.Sleep(100 * time.Millisecond)
		}

		// Check out rev.
		git("checkout", "-q", rev, "--")

		stopTimer := startTimer()
		stopped, err := run1(cmd)
		stopTimer()
		if !pretty {
			fmt.Println()
		}
		if stopped {
			exitStatus = 1
			break
		}
		if err == nil {
			if pretty {
				// Bold green
				printEOL("PASS", "1;32")
			}
		} else {
			// Bold red
			printEOL("FAIL", "1;31")
			exitStatus = 1
		}
		fmt.Println()
	}

	// Clean up
	cleanup()

	os.Exit(exitStatus)
}

func run1(cmd []string) (stopped bool, err error) {
	// Check if we should stop.
	stop.Lock()
	if stop.sig != nil {
		stop.Unlock()
		return true, nil
	}

	c := exec.Command(cmd[0], cmd[1:]...)

	// Open log file for this revision.
	logName := "log." + git("rev-parse", "--short", "HEAD")
	logFile, err := os.Create(logName)
	if err != nil {
		stop.Unlock()
		die("%s", err)
	}
	c.Stdout = logFile
	c.Stderr = logFile
	// Don't leak FDs from git into the subprocess (notably, the
	// git trace FD)
	c.ExtraFiles = make([]*os.File, 100)

	// Start a new process group so we can signal the whole group.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Start command.
	err = c.Start()
	logFile.Close()
	if err == nil {
		stop.proc = c.Process
	}
	stop.Unlock()
	if err != nil {
		die("failed to start command: %s", err)
	}

	// Wait
	err = c.Wait()

	if err == nil {
		os.Remove(logName)
	}

	// Check again for stop and clear process.
	stop.Lock()
	stop.proc = nil
	if stop.sig != nil {
		stop.Unlock()
		return true, nil
	}
	stop.Unlock()
	return false, err
}

func cleanup() {
	git("checkout", "-q", origHEAD)
}

var dying bool

func die(f string, args ...interface{}) {
	if dying {
		os.Exit(1)
	}
	dying = true
	msg := fmt.Sprintf(f, args...)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	os.Stderr.WriteString(msg)
	cleanup()
	os.Exit(1)
}

func tryGit(args ...string) (string, bool) {
	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	if bytes.HasSuffix(out, []byte("\n")) {
		out = out[:len(out)-1]
	}
	return string(out), err == nil
}

func git(args ...string) string {
	out, ok := tryGit(args...)
	if !ok {
		die("git %s failed\n%s", strings.Join(args, " "), out)
	}
	return out
}

func startTimer() func() {
	if !pretty {
		return func() {}
	}
	stopTimerC := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		var now string
		start := time.Now()
		for delta := time.Duration(0); ; delta += time.Second {
			// Delay until delta.
			select {
			case <-time.After(start.Add(delta).Sub(time.Now())):
			case <-stopTimerC:
				// Clear the timer text.
				printEOL(strings.Repeat(" ", len(now)), "")
				wg.Done()
				return
			}
			// Print the time.
			now = fmt.Sprintf("%d:%02d", delta/time.Minute, (delta/time.Second)%60)
			printEOL(now, "")
		}
	}()
	return func() {
		close(stopTimerC)
		wg.Wait()
	}
}

func printEOL(text string, attrs string) {
	if !pretty {
		fmt.Print(text)
		return
	}

	var buf bytes.Buffer
	if attrs != "" {
		fmt.Fprintf(&buf, "\x1b[%sm", attrs)
	}
	// Move to the end of the line, then back up
	// and print text.
	fmt.Fprintf(&buf, "\x1b[999C\x1b[%dD%s", len(text), text)
	if attrs != "" {
		fmt.Fprintf(&buf, "\x1b[0m")
	}

	os.Stdout.Write(buf.Bytes())
}