// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain runs jj-sink itself when JJ_SINK_TEST_MAIN is set in the
// environment, so that repo.sink can run the command by reexecuting
// the test binary. Otherwise it runs the tests as usual.
func TestMain(m *testing.M) {
	if os.Getenv("JJ_SINK_TEST_MAIN") != "" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A repo is a jj repository holding one test's commits.
type repo struct {
	t   *testing.T
	dir string
	env []string
}

// testConfig is the jj configuration the tests run with,
// so that they do not depend on the user's own settings.
const testConfig = `
[user]
name = "Test User"
email = "test@example.com"

[ui]
color = "never"
paginate = "never"
`

// newRepo creates an empty repo in a temporary directory.
func newRepo(t *testing.T) *repo {
	t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj not found in PATH")
	}

	tmp := t.TempDir()
	config := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(config, []byte(testConfig), 0666); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, "repo")
	if err := os.Mkdir(dir, 0777); err != nil {
		t.Fatal(err)
	}

	r := &repo{t: t, dir: dir, env: append(os.Environ(), "JJ_CONFIG="+config)}
	r.jj("git", "init")
	return r
}

// jj runs a jj command in the repo, failing the test if it does not succeed.
func (r *repo) jj(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("jj", args...)
	cmd.Dir = r.dir
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("jj %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// sink runs jj-sink in the repo, returning its combined output and exit status.
func (r *repo) sink(args ...string) (string, int) {
	r.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = r.dir
	cmd.Env = append(r.env, "JJ_SINK_TEST_MAIN=1")
	out, _ := cmd.CombinedOutput()
	r.t.Logf("jj-sink %s\n%s", strings.Join(args, " "), out)
	return string(out), cmd.ProcessState.ExitCode()
}

// commit writes data to file and commits it with the description desc.
// Writing a file that already exists edits it, so that tests can make
// commits that conflict with each other.
func (r *repo) commit(desc, file, data string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, file), []byte(data), 0666); err != nil {
		r.t.Fatal(err)
	}
	r.jj("commit", "-m", desc)
}

// stack returns the descriptions of the repo's commits, topmost first,
// separated by spaces: "C B A" for a stack with A at the bottom.
func (r *repo) stack() string {
	r.t.Helper()
	var descs []string
	out := r.jj("log", "--no-graph", "-r", "all()", "-T", `description.first_line() ++ "\n"`)
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			descs = append(descs, line)
		}
	}
	return strings.Join(descs, " ")
}

// change returns the change id of the commit described by desc.
func (r *repo) change(desc string) string {
	r.t.Helper()
	return r.jj("log", "--no-graph", "-r", "description(glob:"+quote(desc+"*")+")", "-T", "change_id.short()")
}

// commitID returns the commit id of the commit described by desc.
func (r *repo) commitID(desc string) string {
	r.t.Helper()
	return r.jj("log", "--no-graph", "-r", "description(glob:"+quote(desc+"*")+")", "-T", "commit_id.short()")
}

// ops returns the number of operations in the repo's operation log.
func (r *repo) ops() int {
	r.t.Helper()
	return len(strings.Fields(r.jj("op", "log", "--no-graph", "-T", `"x\n"`)))
}

// quote returns s quoted for use in a jj revset.
func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// newStack returns a repo holding the stack "X D C B A", in which
// each commit adds a file of its own, so that X can sink all the way
// to the bottom.
func newStack(t *testing.T) *repo {
	r := newRepo(t)
	for _, desc := range []string{"A", "B", "C", "D", "X"} {
		r.commit(desc, strings.ToLower(desc)+".txt", desc+"\n")
	}
	if got, want := r.stack(), "X D C B A"; got != want {
		t.Fatalf("new stack = %q, want %q", got, want)
	}
	return r
}

func TestSink(t *testing.T) {
	r := newStack(t)
	if out, code := r.sink(r.change("X")); code != 0 {
		t.Fatalf("jj-sink exit status %d, want 0\n%s", code, out)
	}
	if got, want := r.stack(), "D C B A X"; got != want {
		t.Errorf("after sink, stack = %q, want %q", got, want)
	}
}

func TestSinkMax(t *testing.T) {
	r := newStack(t)
	if out, code := r.sink("-m", "2", r.change("X")); code != 0 {
		t.Fatalf("jj-sink exit status %d, want 0\n%s", code, out)
	}
	if got, want := r.stack(), "D C X B A"; got != want {
		t.Errorf("after sink -m 2, stack = %q, want %q", got, want)
	}

	// Sinking again moves the rest of the way.
	r.sink(r.change("X"))
	if got, want := r.stack(), "D C B A X"; got != want {
		t.Errorf("after second sink, stack = %q, want %q", got, want)
	}
}

func TestSinkRev(t *testing.T) {
	// Any revset naming one revision selects the commit to sink.
	for _, test := range []struct {
		name string
		rev  func(r *repo) string
	}{
		{"change", func(r *repo) string { return r.change("X") }},
		{"commit", func(r *repo) string { return r.commitID("X") }},
		{"bookmark", func(r *repo) string { r.jj("bookmark", "create", "-r", r.change("X"), "sinkme"); return "sinkme" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := newStack(t)
			if out, code := r.sink(test.rev(r)); code != 0 {
				t.Fatalf("jj-sink exit status %d, want 0\n%s", code, out)
			}
			if got, want := r.stack(), "D C B A X"; got != want {
				t.Errorf("after sink, stack = %q, want %q", got, want)
			}
		})
	}
}

func TestSinkConflict(t *testing.T) {
	// D edits b.txt, so it can sink past C, which edits a.txt,
	// but not past B, which creates b.txt.
	r := newRepo(t)
	r.commit("A", "a.txt", "a\n")
	r.commit("B", "b.txt", "b\n")
	r.commit("C", "a.txt", "a edited by C\n")
	r.commit("D", "b.txt", "b edited by D\n")

	if out, code := r.sink(r.change("D")); code != 0 {
		t.Fatalf("jj-sink exit status %d, want 0\n%s", code, out)
	}
	if got, want := r.stack(), "C D B A"; got != want {
		t.Errorf("after sink, stack = %q, want %q", got, want)
	}
	if got := r.jj("log", "--no-graph", "-r", "conflicts()", "-T", "change_id.short()"); got != "" {
		t.Errorf("after sink, conflicted revisions = %q, want none", got)
	}
	if got, want := readFile(t, r, "b.txt"), "b edited by D\n"; got != want {
		t.Errorf("after sink, b.txt = %q, want %q", got, want)
	}
}

func TestSinkStuck(t *testing.T) {
	// E edits the same file as D, so it cannot move at all.
	r := newRepo(t)
	r.commit("A", "a.txt", "a\n")
	r.commit("D", "b.txt", "b\n")
	r.commit("E", "b.txt", "b again\n")

	out, code := r.sink(r.change("E"))
	if code != 0 {
		t.Fatalf("jj-sink exit status %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "cannot move down") {
		t.Errorf("jj-sink output = %q, want it to report that E cannot move down", out)
	}
	if got, want := r.stack(), "E D A"; got != want {
		t.Errorf("after sink, stack = %q, want %q", got, want)
	}
}

func TestSinkImmutable(t *testing.T) {
	// X can sink past D and C but not past the immutable B.
	r := newStack(t)
	r.jj("config", "set", "--repo", `revset-aliases."immutable_heads()"`, `description(glob:"B*")`)

	if out, code := r.sink(r.change("X")); code != 0 {
		t.Fatalf("jj-sink exit status %d, want 0\n%s", code, out)
	}
	if got, want := r.stack(), "D C X B A"; got != want {
		t.Errorf("after sink, stack = %q, want %q", got, want)
	}

	// Sinking again does nothing, and does not touch the operation log.
	ops := r.ops()
	if out, code := r.sink(r.change("X")); code != 0 {
		t.Fatalf("jj-sink exit status %d, want 0\n%s", code, out)
	}
	if got, want := r.stack(), "D C X B A"; got != want {
		t.Errorf("after second sink, stack = %q, want %q", got, want)
	}
	if got := r.ops(); got != ops {
		t.Errorf("second sink added %d operations, want 0", got-ops)
	}
}

func TestSinkCollapse(t *testing.T) {
	// A sink of any length is one operation, which one jj undo reverts.
	r := newStack(t)
	ops := r.ops()
	if out, code := r.sink(r.change("X")); code != 0 {
		t.Fatalf("jj-sink exit status %d, want 0\n%s", code, out)
	}
	if got := r.ops(); got != ops+1 {
		t.Errorf("sink added %d operations, want 1", got-ops)
	}
	if got, want := r.jj("op", "log", "-n", "1", "--no-graph", "-T", "self.user()"), "jj-sink@"; !strings.HasPrefix(got, want) {
		t.Errorf("sink operation user = %q, want %q prefix", got, want)
	}

	r.jj("undo")
	if got, want := r.stack(), "X D C B A"; got != want {
		t.Errorf("after undo, stack = %q, want %q", got, want)
	}
	r.jj("redo")
	if got, want := r.stack(), "D C B A X"; got != want {
		t.Errorf("after redo, stack = %q, want %q", got, want)
	}
}

func TestSinkErrors(t *testing.T) {
	r := newStack(t)
	for _, test := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"noargs", nil, 2, "usage: jj-sink"},
		{"twoargs", []string{"a", "b"}, 2, "usage: jj-sink"},
		{"unknown", []string{"nosuchrevision"}, 1, "doesn't exist"},
		{"many", []string{"all()"}, 1, "want 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			out, code := r.sink(test.args...)
			if code != test.code {
				t.Errorf("jj-sink %s exit status %d, want %d\n%s", strings.Join(test.args, " "), code, test.code, out)
			}
			if !strings.Contains(out, test.want) {
				t.Errorf("jj-sink %s output = %q, want it to contain %q", strings.Join(test.args, " "), out, test.want)
			}
		})
	}
	if got, want := r.stack(), "X D C B A"; got != want {
		t.Errorf("after errors, stack = %q, want %q", got, want)
	}
}

// readFile returns the contents of the named file in the repo.
func readFile(t *testing.T, r *repo, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
