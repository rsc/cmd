// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"cmp"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// A Lab holds all the state for a benchmark evaluation.
type Lab struct {
	Commits  []string // -commit
	Hosts    []string // -host
	Reps     int      // -reps
	Pkg      string   // -pkg
	ForceRun bool     // -a

	TestBench     string // -bench (for test binary -test.bench)
	TestBenchtime string // -benchtime (for test binary -test.benchtime)
	TestCount     int    // -count (for test binary -test.count)
	TestCPU       int    // -cpu (for test binary -test.cpu)
	TestRun       string // -run (for test binary -test.run)

	start time.Time

	exec executor    // replaced for testing
	log  *log.Logger // replaced for testing
	fs   fileSystem  // replaced for testing

	gomote *gomoter  // gomote access
	report *reporter // status updates

	hosts    []*host
	machines []*machine
	builds   []*build

	built map[commitBuild]*exe
}

type fileSystem interface {
	Create(name string) (io.WriteCloser, error)
	MkdirAll(name string, mode fs.FileMode) error
	ReadFile(name string) ([]byte, error)
	Remove(name string) error
	Stat(name string) (fs.FileInfo, error)
	WriteFile(name string, data []byte, mode fs.FileMode) error
}

// A build is the configuration for a specific build of the test binary.
type build struct {
	goos   string
	goarch string
	env    []string
	flags  []string
}

// A host is a machine combined with a specific build configuration.
// For example, -host=local:GOAMD64=v2,local:GOAMD64=v3
// lists two separate hosts both referring to the same underlying machine (local).
type host struct {
	name    string   // name qualified by config, like "local:GOAMD64=v2"
	machine *machine // actual machine where things run
	build   *build   // build configuration
}

// A commitBuild is a commit and a build, used as a key for Lab.prog.
type commitBuild struct {
	commit string
	build  *build
}

// An exe is a single built binary.
type exe struct {
	name string
	id   string
}

func (l *Lab) Init(flags *flag.FlagSet) {
	*l = Lab{
		Commits:       []string{"HEAD^", "HEAD"},
		Hosts:         []string{"local"},
		Reps:          4,
		TestBench:     ".",
		TestBenchtime: "500ms",
		TestCount:     5,
		TestRun:       ".",
		exec:          new(localExec),
		log:           log.Default(),
		fs:            new(localFS),
		gomote:        new(gomoter),
		report:        new(reporter),
	}
	if flags != nil {
		flags.Var((*flagStrings)(&l.Commits), "commit", "benchmark commits in `list`")
		flags.Var((*flagStrings)(&l.Hosts), "host", "run benchmarks on hosts in `list`")
		flags.IntVar(&l.Reps, "reps", l.Reps, "run the benchmark program at each commit `R` times")
		flags.StringVar(&l.Pkg, "pkg", "", "benchmark the package at the import `path`")
		flags.BoolVar(&l.ForceRun, "a", false, "force rerun of all tests and benchmarks")
		flags.StringVar(&l.TestBench, "bench", l.TestBench, "run benchmarks with -bench=`pattern`")
		flags.StringVar(&l.TestBenchtime, "benchtime", l.TestBenchtime, "run benchmarks with -benchtime=`d`")
		flags.IntVar(&l.TestCPU, "cpu", l.TestCPU, "run benchmarks with -cpu=`n`")
		flags.IntVar(&l.TestCount, "count", l.TestCount, "run benchmark programs with -count=`N`")
		flags.StringVar(&l.TestRun, "run", l.TestRun, "run tests with -run=`pattern`")
	}
}

func (l *Lab) Run() error {
	steps := []func() error{
		l.gitResolve,
		l.scanHosts,
		l.build,
		l.runAll,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func (l *Lab) Stats() string {
	return l.report.stats
}

type localFS struct{}

func (*localFS) Create(name string) (io.WriteCloser, error) {
	f, err := os.Create(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (*localFS) MkdirAll(name string, mode fs.FileMode) error {
	return os.MkdirAll(name, mode)
}

func (*localFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(name)
}

func (*localFS) Remove(name string) error {
	return os.Remove(name)
}

func (*localFS) Stat(name string) (fs.FileInfo, error) {
	return os.Stat(name)
}

func (*localFS) WriteFile(name string, data []byte, mode fs.FileMode) error {
	return os.WriteFile(name, data, mode)
}

// hash returns a short hash of its inputs.
func hash(args ...any) string {
	sum := sha256.Sum256([]byte(fmt.Sprintln(args...)))
	return fmt.Sprintf("%x", sum[:6])
}

// stringList flattens its arguments into a single []string.
// Each argument in args must have type string or []string.
func stringList(args ...any) []string {
	var x []string
	for _, arg := range args {
		switch arg := arg.(type) {
		case []string:
			x = append(x, arg...)
		case string:
			x = append(x, arg)
		default:
			panic("stringList: invalid argument of type " + fmt.Sprintf("%T", arg))
		}
	}
	return x
}

// flagStrings is a slice of strings that can be used as a flag.
type flagStrings []string

func (s *flagStrings) String() string {
	return strconv.Quote(strings.Join(*s, ","))
}

func (s *flagStrings) Set(value string) error {
	*s = strings.Split(value, ",")
	return nil
}

func parDo[T any](l *Lab, xs []T, fn func(T) error) error {
	done := make(chan error, len(xs))
	for _, x := range xs {
		go func() {
			err := fn(x)
			if err != nil {
				l.log.Print(err)
			}
			done <- err
		}()
	}

	var errs []error
	for range xs {
		errs = append(errs, <-done)
	}
	return cmp.Or(errs...)
}
