// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Running on machines.

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// A machine represents a single system that runs tests and benchmarks.
type machine struct {
	name   string // name of machine
	kind   string // local, ssh, or gomote
	goos   string // target goos
	goarch string // target goarch
	cpu    int    // number of CPUs (cores)
	jobs   []*job // jobs to run on machine

	gomoteKind string // gomote type to use
	gomoteName string // gomote instance name
}

// A runMode controls the details of running a command.
type runMode int

const (
	_         runMode = 1 << iota
	runTrim           // trim spaces in output
	runStderr         // include stderr in output
)

// An executor runs commands.
type executor interface {
	// run has the same semantics as runLocal,
	// except that it need not handle runTrim.
	run(mode runMode, cmd ...string) (out string, err error)
}

// runLocal runs cmd on the local system according to mode.
// If the command fails, runLocal returns an empty output
// and an error message that contains both stdout and stderr.
// If mode has the runTrim bit set, runLocal trims leading and trailing spaces from the output.
// If mode has the runStderr bit set, then stderr is included in the output on success
// rather than being discarded.
func (l *Lab) runLocal(mode runMode, cmd ...string) (out string, err error) {
	out, err = l.exec.run(mode&^runTrim, cmd...)
	if mode&runTrim != 0 {
		out = strings.TrimSpace(out)
	}
	return out, err
}

// A localExec is an executor that runs commands locally.
// It is replaced in tests to avoid needing to run actual commands.
type localExec struct{}

func (*localExec) run(mode runMode, cmd ...string) (out string, err error) {
	if len(cmd) == 0 {
		return "", fmt.Errorf("missing command")
	}
	orig := cmd
	var env []string
	for len(cmd) > 0 && strings.Contains(cmd[0], "=") {
		if env == nil {
			env = os.Environ()
		}
		env = append(env, cmd[0])
		cmd = cmd[1:]
	}
	if len(cmd) == 0 {
		return "", fmt.Errorf("command entirely environment: %s", strings.Join(orig, " "))
	}

	c := exec.Command(cmd[0], cmd[1:]...)
	c.Env = env

	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if mode&runStderr != 0 {
		c.Stderr = &stdout // merge stdout and stderr
	}
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("%s: %s\n%s%s", strings.Join(orig, " "), err, stdout.Bytes(), stderr.Bytes())
	}
	return stdout.String(), nil
}

// scanHosts reads the -host list and constructs the host, machine,
// and build data structures describing the hosts to be used.
func (l *Lab) scanHosts() error {
	machines := make(map[string]*machine)
	for _, hostname := range l.Hosts {
		name, _, _ := strings.Cut(hostname, ":")
		m := machines[name]
		if m == nil {
			m = &machine{name: name}
			machines[name] = m
			l.machines = append(l.machines, m)
		}
		l.hosts = append(l.hosts, &host{name: hostname, machine: m})
	}

	if err := parDo(l, l.machines, l.scanMachine); err != nil {
		// parDo already printed errors, so don't repeat them.
		return fmt.Errorf("scanning hosts failed")
	}

	builds := make(map[string]*build)
	for _, h := range l.hosts {
		goos := h.machine.goos
		goarch := h.machine.goarch
		var env, flags []string
		for _, opt := range strings.Split(h.name, ":")[1:] {
			k, v, ok := strings.Cut(opt, "=")
			if !ok || k == "" || v == "" {
				return fmt.Errorf("invalid config %q in host name: %v", opt, h.name)
			}
			switch {
			case k == "GOOS":
				goos = v
			case k == "GOARCH":
				goarch = v
			case k == strings.ToUpper(k):
				env = append(env, k+"="+v)
			default:
				flags = append(flags, "-"+k, v)
			}
		}
		key := fmt.Sprintf("%q %q %q %q", goos, goarch, env, flags)
		b := builds[key]
		if b == nil {
			b = &build{
				goos:   goos,
				goarch: goarch,
				env:    env,
				flags:  flags,
			}
			builds[key] = b
			l.builds = append(l.builds, b)
		}
		h.build = b
	}
	return nil
}

// scanMachine initializes the goos and goarch for a single machine.
//
// For ssh-able machines, scanMachine checks that ssh works,
// since it needs to ssh in to find the goos and goarch.
//
// For gomotes, scanMachine determines the kind of gomote
// but does not create one yet, since the goos and goarch are
// evident from the name. This avoids creating gomotes
// (which can be very slow) when all the jobs from that gomote
// are already cached.
func (l *Lab) scanMachine(m *machine) error {
	if m.name == "local" {
		m.kind = "local"
		return l.scanArch(m)
	}
	for _, goos := range gooses {
		if strings.HasPrefix(m.name, goos+"-") {
			goos, goarch, _ := strings.Cut(m.name, "-")
			goarch, _, _ = strings.Cut(goarch, "-") // linux-amd64-longtest
			goarch, _, _ = strings.Cut(goarch, "_") // darwin-arm64_14
			m.kind = "gomote"
			m.goos = goos
			m.goarch = goarch
			return l.gomote.scan(l, m)
		}
	}
	m.kind = "ssh"
	if _, err := l.runRemote(m, 0, "date"); err != nil {
		return err
	}
	return l.scanArch(m)
}

// scanArch determines the GOOS and GOARCH for the machine.
func (l *Lab) scanArch(m *machine) error {
	out, err := l.runRemote(m, runTrim, "uname")
	if err != nil {
		return err
	}
	var ok bool
	if m.goos, ok = goosByUname[out]; !ok {
		return fmt.Errorf("unknown uname: %s", out)
	}
	out, err = l.runRemote(m, runTrim, "uname", "-m")
	if err != nil {
		return err
	}
	if m.goarch, ok = goarchByUname[out]; !ok {
		return fmt.Errorf("unknown uname -m: %s", out)
	}
	return nil
}

// scanNumCPU determines the number of CPUs for the machine.
func (l *Lab) scanNumCPU(m *machine) error {
	var cmd []string
	switch m.goos {
	default:
		return fmt.Errorf("cannot count CPUs on GOOS=%s", m.goos)
	case "linux":
		cmd = []string{"nproc"}
	case "darwin", "freebsd", "openbsd", "netbsd", "dragonfly":
		cmd = []string{"sysctl", "hw.ncpu"}
	case "windows":
		cmd = []string{"wmic", "cpu", "get", "NumberOfCores"}
	}
	out, err := l.runRemote(m, 0, cmd...)
	if err != nil {
		return err
	}

	// Use last space-separated field, to skip leading chatter
	// like "hw.ncpu:" or "NumberOfCores".
	f := strings.Fields(out)
	if len(f) == 0 {
		return fmt.Errorf("'%s' on %s: no output", strings.Join(cmd, " "), m.name)
	}
	n, err := strconv.Atoi(f[len(f)-1])
	if err != nil {
		return fmt.Errorf("'%s' on %s: unexpected output:\n%s", strings.Join(cmd, " "), m.name, out)
	}
	m.cpu = n
	return nil
}

func (l *Lab) upload(m *machine, files []string) error {
	switch m.kind {
	case "ssh":
		_, err := l.runLocal(0, stringList("scp", files, m.name+":")...)
		return err
	case "gomote":
		for _, file := range files {
			if _, err := l.runLocal(0, "gomote", "put", m.gomoteName, file); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *Lab) runRemote(m *machine, mode runMode, cmd ...string) (out string, err error) {
	switch m.kind {
	case "ssh":
		// TODO quote cmd
		cmd = stringList("ssh", m.name, cmd)
	case "gomote":
		// TODO quote cmd
		cmd = stringList("gomote", "run", m.gomoteName, cmd)
	}
	return l.runLocal(mode, cmd...)
}

// A gomoter provides access to gomotes.
type gomoter struct {
	initOnce    sync.Once
	connectOnce sync.Once
	kinds       []string
	kindsErr    error
	motes       map[string][]string
	motesErr    error

	mu sync.Mutex // for connect
}

// init initializes the list of known gomote kinds and active available gomotes.
func (g *gomoter) init(l *Lab) error {
	g.initOnce.Do(func() {
		// Load the set of gomote builds.
		out, err := l.runLocal(0, "gomote", "create", "-list")
		g.kindsErr = err
		g.kinds = strings.Fields(out)
	})
	return g.kindsErr
}

func (g *gomoter) connect(l *Lab, m *machine) error {
	g.connectOnce.Do(func() {
		// Create the benchlab group if it doesn't exist.
		// (If it does exist, ignore the error.)
		l.runLocal(0, "gomote", "group", "create", "benchlab")

		// List the existing motes for reuse,
		// but only in the benchlab group.
		out, err := l.runLocal(0, "gomote", "list")
		g.motesErr = err
		g.motes = make(map[string][]string)
		for line := range strings.Lines(out) {
			f := strings.Fields(line)
			if len(f) >= 3 && f[1] == "(benchlab)" {
				mote := f[0]
				kind := f[2]
				g.motes[kind] = append(g.motes[kind], mote)
			}
		}
	})
	if g.motesErr != nil {
		return g.motesErr
	}

	// If there is an existing gomote, take it.
	g.mu.Lock()
	motes := g.motes[m.gomoteKind]
	if len(motes) > 0 {
		m.gomoteName, g.motes[m.gomoteKind] = motes[0], motes[1:]
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()

	name, err := l.runLocal(runTrim, "gomote", "-group=benchlab", "create", m.gomoteKind)
	if err != nil {
		return err
	}
	m.gomoteName = name
	return nil
}

// scan finds the gomote kind that should be used for m.
func (g *gomoter) scan(l *Lab, m *machine) error {
	if err := g.init(l); err != nil {
		return err
	}
	want := "gotip-" + m.name
	wantLast := false
	if m.name == "darwin-arm64" || m.name == "darwin-amd64" {
		// darwin gomotes end in _14, _15 etc for the darwin version.
		// Take the last one in the list, which should be the biggest version.
		want += "_"
		wantLast = true
	}
	kind := ""
	for _, k := range g.kinds {
		if strings.HasPrefix(k, want) {
			kind = k
			if !wantLast {
				break
			}
		}
	}
	if kind == "" {
		return fmt.Errorf("no gomote kind for %s", m.name)
	}
	m.gomoteKind = kind
	return nil
}

// goosByUname maps "uname" output to GOOS.
var goosByUname = map[string]string{
	"Linux":     "linux",
	"Darwin":    "darwin",
	"FreeBSD":   "freebsd",
	"OpenBSD":   "openbsd",
	"NetBSD":    "netbsd",
	"DragonFly": "dragonfly",
	"Windows":   "windows",
}

// goarchByUname maps "uname -m" output to GOOS.
var goarchByUname = map[string]string{
	"x86_64":   "amd64",
	"amd64":    "amd64",
	"arm64":    "arm64",
	"aarch64":  "arm64",
	"arm":      "arm",
	"i386":     "386",
	"i686":     "386",
	"x86":      "386",
	"mips":     "mips",
	"mips64":   "mips64",
	"mips64le": "mips64le",
	"mipsle":   "mipsle",
	"ppc64":    "ppc64",
	"ppc64le":  "ppc64le",
	"s390x":    "s390x",
}

// gooses is the list of known GOOS values.
var gooses = []string{
	"darwin",
	"dragonfly",
	"freebsd",
	"linux",
	"netbsd",
	"openbsd",
	"solaris",
	"windows",
}
