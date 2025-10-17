// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Running benchmarks.

package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// A job is a single run of a program on a host.
type job struct {
	parent  *job      // parent job that must succeed first
	done    chan bool // closed when job is done
	host    *host     // host being used
	commit  string    // commit being run
	exe     *exe      // executable to run
	args    []string  // arguments to executable
	phase   int       // phase (0=test, 1,2,3,...=rep)
	success bool      // whether the job passed
	out     string    // output from job
	cache   string    // output cache file
}

func (j *job) String() string {
	name := j.host.name + "@" + j.commit
	if j.phase == 0 {
		name += " (test)"
	} else {
		name += fmt.Sprintf(" #%d", j.phase)
	}
	return name
}

// A reporter reports status updates.
type reporter struct {
	started time.Time

	mu         sync.Mutex // only needed after r.start
	jobsCached int
	jobsDone   int
	jobsTotal  int
	rawFile    string         // path to benchmark output file
	rawOut     io.WriteCloser // raw benchmark output
	stats      string         // benchstat output
	statFile   string         // path to benchstat output file
	statCmd    []string       // command to refresh benchstat output
}

func (l *Lab) runAll() error {
	// Choose output file, avoiding existing files.
	date := time.Now().Format("2006-01-02")
	var rawFile string
	for i := 0; ; i++ {
		suffix := ""
		if i > 0 {
			suffix = fmt.Sprintf(".%d", i)
		}
		rawFile = "bench." + date + suffix + ".txt"
		if _, err := l.fs.Stat(rawFile); err != nil {
			l.report.statFile = "benchstat." + date + suffix + ".txt"
			break
		}
	}

	f, err := l.fs.Create(rawFile)
	if err != nil {
		return err
	}
	l.report.rawFile = rawFile
	l.report.rawOut = f

	// Choose benchstat layout.
	// TODO: Find highest priority axis with variation.
	bcmd := []string{"benchstat", "-alpha=0.001", "-col=commit", "-table=host"}
	l.report.statCmd = append(bcmd, rawFile)

	// Make list of job by host, loading cached results if available.
	cpuArgs := []string{}
	if l.TestCPU > 0 {
		cpuArgs = []string{fmt.Sprintf("-test.cpu=%d", l.TestCPU)}
	}
	testArgs := slices.Clip(append(cpuArgs,
		fmt.Sprintf("-test.run=%s", l.TestRun),
	))
	benchArgs := slices.Clip(append(cpuArgs,
		"-test.run=^$",
		fmt.Sprintf("-test.bench=%s", l.TestBench),
		fmt.Sprintf("-test.count=%d", l.TestCount),
		fmt.Sprintf("-test.benchtime=%s", l.TestBenchtime),
	))

	// Two phases: tests, then benchmarks.
	var tests []*job
	for phase := range 1 + l.Reps {
		id := 0
		for _, commit := range l.Commits {
			for _, h := range l.hosts {
				prog := l.built[commitBuild{commit, h.build}]
				if prog == nil {
					return fmt.Errorf("missing exe for %s@%s", h.name, commit)
				}
				j := &job{
					commit: commit,
					host:   h,
					exe:    prog,
					phase:  phase,
					done:   make(chan bool),
				}
				if phase == 0 {
					j.args = testArgs
					tests = append(tests, j)
				} else {
					j.args = benchArgs
					j.parent = tests[id]
				}
				id++
				j.cache = ".benchlab/cache." + hash(prog.id, h.machine.name, j.args, j.phase) + ".txt"
				if out, err := l.fs.ReadFile(j.cache); err == nil && len(out) > 0 && !l.ForceRun {
					j.success = true
					j.out = string(out)
					close(j.done)
					l.report.jobsCached++
					l.report.done(l, j)
					continue
				}
				h.machine.jobs = append(h.machine.jobs, j)
				l.report.jobsTotal++
			}
		}
	}

	l.log.Printf("running benchmarks; tail -F %s for updates", l.report.statFile)
	l.report.start(l)

	if err := parDo(l, l.machines, l.runMachine); err != nil {
		return err
	}

	l.report.writeStat(l) // in case it was 100% cached
	l.log.Printf("completed!")
	return nil
}

func (l *Lab) runMachine(m *machine) error {
	// If all the jobs had cached runs, stop.
	if len(m.jobs) == 0 {
		return nil
	}

	// Allocate gomote if needed.
	if m.kind == "gomote" {
		if err := l.gomote.connect(l, m); err != nil {
			return err
		}
	}

	// Count CPUs.
	if err := l.scanNumCPU(m); err != nil {
		return err
	}

	// Copy all binaries to machine.
	need := make(map[string]bool)
	for _, j := range m.jobs {
		need[j.exe.name] = true
	}
	if err := l.upload(m, slices.Sorted(maps.Keys(need))); err != nil {
		return err
	}

	// Determine how many jobs can run at once.
	maxJobs := 1
	if l.TestCPU > 0 && m.cpu > 0 {
		maxJobs = max(1, m.cpu/l.TestCPU)
	}

	// Run them all.
	done := make(chan *job, len(m.jobs))
	active := 0
	for _, j := range m.jobs {
		if active == maxJobs {
			l.report.done(l, <-done)
			active--
		}
		go func() {
			l.runJob(j, done)
			close(j.done)
			done <- j
		}()
		active++
	}
	for range active {
		l.report.done(l, <-done)
	}
	return nil
}

func (l *Lab) runJob(j *job, done chan<- *job) {
	if j.parent != nil {
		if <-j.parent.done; !j.parent.success {
			l.log.Printf("%s: skipping because test failed", j)
			return
		}
	}

	prog := j.exe.name
	if j.host.machine.kind != "local" {
		prog = "./" + filepath.Base(prog)
	}
	out, err := l.runRemote(j.host.machine, 0, append([]string{prog}, j.args...)...)
	if err != nil {
		l.log.Printf("%s: %s", j, err)
		return
	}
	j.success = true
	j.out = out
	if err := l.fs.WriteFile(j.cache, []byte(out), 0666); err != nil {
		l.log.Printf("%s: %s", j, err)
	}
}

func (r *reporter) start(l *Lab) {
	l.log.Printf("[0/%d 0s] reused %d cached runs; starting new runs", r.jobsTotal, r.jobsCached)
	r.started = time.Now()
}

func (r *reporter) done(l *Lab, j *job) {
	fmt.Fprintf(r.rawOut, "# %s\n\nhost: %s\ncommit: %s\n\n%s\n", j, j.host.name, j.commit, j.out)
	if r.started.IsZero() {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobsDone++
	r.writeStat(l)

	l.log.Printf("[%d/%d %v] %s done", r.jobsDone, r.jobsTotal, time.Since(r.started).Round(time.Second), j)
}

func (r *reporter) writeStat(l *Lab) {
	stats, err := l.runLocal(0, r.statCmd...)
	if err != nil {
		l.log.Print(err)
		return
	}
	r.stats = stats

	if len(l.Commits) == 2 {
		txt, err := l.runLocal(0, stringList("benchstat", "-format=csv", r.statCmd[1:])...)
		if err != nil {
			l.log.Print(err)
			return
		}
		tab, err := csvToTable(txt)
		if err != nil {
			l.log.Print(err)
			return
		}
		r.stats += "\n" + tab
	}

	// Write benchstat file.
	// Remove before WriteFile makes tail -F see the file as worth reprinting anew.
	data := fmt.Appendf(nil, "# %s\n\n%s", strings.Join(r.statCmd, " "), r.stats)
	l.fs.Remove(r.statFile)
	if err := l.fs.WriteFile(r.statFile, data, 0666); err != nil {
		l.log.Print(err)
	}
}

func csvToTable(txt string) (string, error) {
	rd := csv.NewReader(strings.NewReader(txt))
	rd.FieldsPerRecord = -1
	recs, err := rd.ReadAll()
	if err != nil {
		return "", err
	}

	var hosts, names []string
	known := make(map[string]bool)
	delta := make(map[[2]string]string)
	for len(recs) > 0 && (len(recs[0]) < 1 || !strings.HasPrefix(recs[0][0], "host:")) {
		recs = recs[1:]
	}
	for len(recs) > 0 {
		host := strings.TrimPrefix(recs[0][0], "host: ")
		hosts = append(hosts, host)
		i := 1
		for i < len(recs) && (len(recs[i]) < 1 || !strings.HasPrefix(recs[i][0], "host:")) {
			i++
		}
		chunk := recs[:i]
		recs = recs[i:]

		for len(chunk) > 0 && (len(chunk[0]) < 2 || chunk[0][1] != "sec/op") {
			chunk = chunk[1:]
		}
		for len(chunk) > 0 && len(chunk[0]) >= 6 && chunk[0][0] != "geomean" {
			line := chunk[0]
			chunk = chunk[1:]
			name := line[0]
			i := strings.LastIndex(name, "-")
			if i >= 0 {
				name = name[:i] // chop CPU
			}
			if !known[name] {
				names = append(names, name)
				known[name] = true
			}
			delta[[2]string{host, name}] = line[5]
		}
	}

	table := [][]string{stringList(`benchmark \ host`, hosts)}
	for _, name := range names {
		row := []string{name}
		for _, host := range hosts {
			d := delta[[2]string{host, name}]
			if d == "" {
				d = "?"
			}
			row = append(row, d)
		}
		table = append(table, row)
	}

	var max []int
	for _, row := range table {
		for i, c := range row {
			n := utf8.RuneCountInString(c)
			if i >= len(max) {
				max = append(max, n)
			} else if max[i] < n {
				max[i] = n
			}
		}
	}

	var out bytes.Buffer
	b := bufio.NewWriter(&out)
	for _, row := range table {
		for len(row) > 0 && row[len(row)-1] == "" {
			row = row[:len(row)-1]
		}
		for i, c := range row {
			if i > 0 {
				for j := utf8.RuneCountInString(c); j < max[i]+2; j++ {
					b.WriteRune(' ')
				}
			}
			b.WriteString(c)
			if i == 0 && i+1 < len(row) {
				for j := utf8.RuneCountInString(c); j < max[i]+2; j++ {
					b.WriteRune(' ')
				}
			}
		}
		b.WriteRune('\n')
	}
	b.Flush()

	return out.String(), nil
}
