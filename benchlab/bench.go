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
	"strconv"
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
	// Name the binary, so that a surprising result can be traced back to the
	// exact file in .benchlab and disassembled.
	if j.exe != nil {
		name += " " + filepath.Base(j.exe.name)
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
	layouts    map[layoutKey]map[int][]float64 // ns/op samples by layout seed
	rawFile    string                          // path to benchmark output file
	rawOut     io.WriteCloser                  // raw benchmark output
	stats      string                          // benchstat output
	statFile   string                          // path to benchstat output file
	statCmd    []string                        // command to refresh benchstat output
}

func joinQuoted(s []string) string {
	q := fmt.Sprintf("%q", s)
	return q[1 : len(q)-1]
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
		rawFile = ".benchlab/bench." + date + suffix + ".txt"
		if _, err := l.fs.Stat(rawFile); err != nil {
			l.report.statFile = ".benchlab/benchstat." + date + suffix + ".txt"
			break
		}
	}

	f, err := l.fs.Create(rawFile)
	if err != nil {
		return err
	}
	l.report.rawFile = rawFile
	l.report.rawOut = f

	// Write commits and hosts in command-line order for benchstat,
	// in case the actual results come in reordered due to parallelism.
	for _, commit := range l.Commits {
		fmt.Fprintf(f, "commit: %s\n", commit)
	}
	for _, host := range l.Hosts {
		fmt.Fprintf(f, "host: %s\n", host)
	}

	// Choose benchstat layout.
	// TODO: Find highest priority axis with variation.
	bcmd := []string{
		"benchstat", "-alpha=0.001",
		// benchstat's default row projection is .fullname, which keeps the
		// -N GOMAXPROCS suffix on every benchmark name. Drop it: the host
		// column already says which machine, and N follows from that.
		"-row=.name",
		fmt.Sprintf("-col=commit@(%s)", joinQuoted(l.Commits)),
		fmt.Sprintf("-table=host@(%s)", joinQuoted(l.Hosts)),
	}
	if l.RandLayout {
		// Pool the layouts instead of reporting each one separately:
		// averaging over them is the entire point of varying them.
		bcmd = append(bcmd, "-ignore=randlayout")
	}
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
				exes := l.built[commitBuild{commit, h.build}]
				if len(exes) == 0 {
					return fmt.Errorf("missing exe for %s@%s", h.name, commit)
				}
				// Each rep runs a differently laid out binary; the tests,
				// which only need to pass, run the first one.
				prog := exes[0]
				if phase > 0 {
					prog = exes[(phase-1)%len(exes)]
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
	l.report.writeStat(l) // in case it was 100% cached
	l.log.Printf("[0/%d 0s] reused %d cached runs; starting new runs", r.jobsTotal, r.jobsCached)
	r.started = time.Now()
}

func (r *reporter) done(l *Lab, j *job) {
	// Hold the lock for the raw output too: jobs on different machines
	// finish concurrently, and their blocks must not interleave.
	r.mu.Lock()
	// Reset cpu/goos/goarch/pkg explicitly: benchfmt configuration keys are
	// sticky across sections, so e.g. an amd64 section's "cpu: Intel..."
	// would otherwise leak into a following arm64 section that doesn't
	// emit its own cpu: line, making benchstat warn that benchmarks vary
	// in cpu within a single (host, commit) group.
	fmt.Fprintf(r.rawOut, "# %s\n\nhost: %s\ncommit: %s\ncpu:\ngoos:\ngoarch:\npkg:\n", j, j.host.name, j.commit)
	if j.exe != nil && j.exe.seed != 0 {
		fmt.Fprintf(r.rawOut, "randlayout: %d\n", j.exe.seed)
	}
	fmt.Fprintf(r.rawOut, "\n%s\n", j.out)
	r.recordLayout(j)
	r.mu.Unlock()

	// Cached runs are reported before r.start, when there is nothing to
	// update yet. Their samples are recorded above all the same.
	if r.started.IsZero() {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobsDone++
	r.writeStat(l)

	l.log.Printf("[%d/%d %v] %s done", r.jobsDone, r.jobsTotal, time.Since(r.started).Round(time.Second), j)
}

// A layoutKey identifies one benchmark's samples on one host at one commit.
type layoutKey struct {
	host   string
	commit string
	bench  string
}

// layoutSensitive is how far one layout's median must sit from the median
// across all layouts before the report calls the benchmark out.
const layoutSensitive = 0.10

// recordLayout files j's benchmark results under the layout seed that produced
// them, so that layoutNotes can tell a benchmark whose speed depends on the
// linker's layout from one whose speed does not.
// r.mu must be held.
func (r *reporter) recordLayout(j *job) {
	if j.exe == nil || j.exe.seed == 0 || !j.success {
		return
	}
	for line := range strings.Lines(j.out) {
		name, ns, ok := benchNsPerOp(line)
		if !ok {
			continue
		}
		if r.layouts == nil {
			r.layouts = make(map[layoutKey]map[int][]float64)
		}
		k := layoutKey{j.host.name, j.commit, name}
		if r.layouts[k] == nil {
			r.layouts[k] = make(map[int][]float64)
		}
		r.layouts[k][j.exe.seed] = append(r.layouts[k][j.exe.seed], ns)
	}
}

// benchNsPerOp parses a testing benchmark result line, returning the benchmark
// name and its ns/op value. The name drops both the "Benchmark" prefix and the
// trailing -N GOMAXPROCS suffix, to match the names benchstat prints.
func benchNsPerOp(line string) (name string, ns float64, ok bool) {
	f := strings.Fields(line)
	if len(f) < 4 || !strings.HasPrefix(f[0], "Benchmark") {
		return "", 0, false
	}
	// Fields after the iteration count are value/unit pairs.
	for i := 2; i+1 < len(f); i += 2 {
		if f[i+1] == "ns/op" {
			v, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				return "", 0, false
			}
			return benchName(f[0]), v, true
		}
	}
	return "", 0, false
}

// benchName trims a benchmark's "Benchmark" prefix and -N GOMAXPROCS suffix.
func benchName(name string) string {
	name = strings.TrimPrefix(name, "Benchmark")
	if i := strings.LastIndex(name, "-"); i > 0 {
		if n := name[i+1:]; n != "" && strings.Trim(n, "0123456789") == "" {
			name = name[:i]
		}
	}
	return name
}

// layoutNotes returns a footnote naming the benchmarks whose speed depends on
// which linker layout they were built with: those where some seed's median sits
// more than layoutSensitive away from the median across all seeds.
//
// Pooling the layouts keeps the comparison between commits honest, because
// every commit is measured over the same set of seeds. But the interval printed
// with each number is a confidence interval for the median, and a layout that
// is slow in a minority of runs does not move the median at all, so the table
// above gives no hint that the benchmark is sensitive. Hence this note.
func (r *reporter) layoutNotes() string {
	type note struct {
		key   layoutKey
		seed  int
		frac  float64
		seeds int
	}
	var notes []note
	for k, bySeed := range r.layouts {
		if len(bySeed) < 2 {
			continue
		}
		var all []float64
		for _, vs := range bySeed {
			all = append(all, vs...)
		}
		pooled := median(all)
		if pooled == 0 {
			continue
		}
		for seed, vs := range bySeed {
			if frac := median(vs)/pooled - 1; frac >= layoutSensitive || frac <= -layoutSensitive {
				notes = append(notes, note{k, seed, frac, len(bySeed)})
			}
		}
	}
	if len(notes) == 0 {
		return ""
	}
	slices.SortFunc(notes, func(a, b note) int {
		if c := strings.Compare(a.key.host, b.key.host); c != 0 {
			return c
		}
		if c := strings.Compare(a.key.commit, b.key.commit); c != 0 {
			return c
		}
		if c := strings.Compare(a.key.bench, b.key.bench); c != 0 {
			return c
		}
		return a.seed - b.seed
	})

	var buf strings.Builder
	fmt.Fprintf(&buf, "\nWarning: layout-sensitive benchmarks detected:\n\n")
	for _, n := range notes {
		fmt.Fprintf(&buf, "* %s %s %s: seed %d %+.0f%% (of %d seeds)\n",
			n.key.host, n.key.commit, n.key.bench, n.seed, 100*n.frac, n.seeds)
	}
	return buf.String()
}

// median returns the median of xs, or 0 if xs is empty.
func median(xs []float64) float64 {
	s := slices.Sorted(slices.Values(xs))
	if len(s) == 0 {
		return 0
	}
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
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
	data = append(data, r.layoutNotes()...)
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
