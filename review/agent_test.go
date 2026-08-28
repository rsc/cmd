// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureOut redirects the command output for the duration of a test.
func captureOut(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := stdout
	stdout = &buf
	t.Cleanup(func() { stdout = old })
	return &buf
}

func TestSkillPrint(t *testing.T) {
	out := captureOut(t)
	cmdSkill(nil)
	got := out.String()

	// -print must emit exactly what -install writes, frontmatter included,
	// so that the two can never drift.
	if got != skillText() {
		t.Errorf("printed text is not the skill that would be installed")
	}
	if !strings.HasPrefix(got, "---\nname: review\n") {
		t.Errorf("skill has no frontmatter:\n%s", got[:min(200, len(got))])
	}
	// The instructions have to name the commands an agent actually runs.
	for _, want := range []string{
		"review comments",
		"review reply -resolve 12",
		"review reply -from claude",
		"review add CHANGE file.go:42",
		"review snapshot",
		"do not pass -drafts",
		"Never resolve a thread you did not address.",
		// And that a question about a comment is a reply on its thread,
		// not a halt to ask the user, which strands the other threads.
		"Ask through the review, not in conversation.",
		"Never stop the work to ask about a comment.",
		// The instructions must warn that a comment's line moves, and
		// that the context is the older text, which are the two things
		// an agent gets wrong if it is not told.
		"written at line N",
		// And in what order to work through a stack of commits.
		"from the oldest to the newest",
		// And how to make a fix in jj so that a wrong one can be dropped
		// without going through the operation log.
		"jj squash",
		`Not "jj edit K".`,
		"now gone",
		"text as the reviewer saw it",
		"current_line",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("agent instructions missing %q", want)
		}
	}
}

// TestSkillInstallDetectsAgents checks that -install writes only for the
// agents actually set up on the machine.
func TestSkillInstallDetectsAgents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// With nothing installed, nothing is written.
	out := captureOut(t)
	cmdSkill([]string{"-install"})
	if !strings.Contains(out.String(), "no coding agents found") {
		t.Errorf("expected no agents to be found:\n%s", out.String())
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Errorf("wrote into an empty home directory: %v", entries)
	}

	// Claude Code present: a real skill file.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0777); err != nil {
		t.Fatal(err)
	}
	out = captureOut(t)
	cmdSkill([]string{"-install"})
	if !strings.Contains(out.String(), "Claude Code") {
		t.Errorf("Claude Code not reported:\n%s", out.String())
	}
	skill := filepath.Join(home, ".claude", "skills", "review", "SKILL.md")
	data, err := os.ReadFile(skill)
	if err != nil {
		t.Fatalf("no skill installed: %v", err)
	}
	if string(data) != skillText() {
		t.Error("installed skill differs from the printed one")
	}
	// A tool that is not set up is left alone.
	if _, err := os.Stat(filepath.Join(home, ".gemini")); !os.IsNotExist(err) {
		t.Error("created a config directory for a tool that is not installed")
	}

	// Antigravity present: a real skill in ~/.gemini/config/skills, which
	// is the layout its documentation gives.
	if err := os.MkdirAll(filepath.Join(home, ".gemini"), 0777); err != nil {
		t.Fatal(err)
	}
	out = captureOut(t)
	cmdSkill([]string{"-install"})
	if !strings.Contains(out.String(), "Antigravity") {
		t.Errorf("Antigravity not reported:\n%s", out.String())
	}
	gemSkill := filepath.Join(home, ".gemini", "config", "skills", "review", "SKILL.md")
	data, err = os.ReadFile(gemSkill)
	if err != nil {
		t.Fatalf("no Antigravity skill installed: %v", err)
	}
	if string(data) != skillText() {
		t.Error("Antigravity skill differs from the printed one")
	}
	// Nothing is added to any always-on instructions file.
	for _, name := range []string{"GEMINI.md", "AGENTS.md", "CLAUDE.md"} {
		for _, dir := range []string{home, filepath.Join(home, ".gemini"), filepath.Join(home, ".claude")} {
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Errorf("wrote %s, want skills only", filepath.Join(dir, name))
			}
		}
	}
}

// TestSkillInstallProject checks that -install writes into the repository,
// at each tool's own workspace path: Antigravity's is .agents/skills, not
// the same as Claude Code's.
// TestSkillInstallProject checks that -install writes into the repository
// at each tool's own workspace path, and that a path two tools share is
// written once and reported as serving both.
func TestSkillInstallProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, d := range []string{".claude", ".gemini", ".codex"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0777); err != nil {
			t.Fatal(err)
		}
	}
	repo := newGitRepo(t)
	t.Chdir(repo)

	out := captureOut(t)
	cmdSkill([]string{"-install", "-project"})

	for _, path := range []string{
		filepath.Join(repo, ".claude", "skills", "review", "SKILL.md"),
		filepath.Join(repo, ".agents", "skills", "review", "SKILL.md"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("no skill at %s: %v", path, err)
			continue
		}
		if string(data) != skillText() {
			t.Errorf("skill at %s differs from the printed one", path)
		}
	}
	// Only skills, no instructions files.
	if _, err := os.Stat(filepath.Join(repo, "GEMINI.md")); !os.IsNotExist(err) {
		t.Error("wrote GEMINI.md into the repository, want skills only")
	}
	// Antigravity and Codex share .agents/skills, so it is written once
	// and reported as serving both rather than listed twice.
	agents := filepath.Join(repo, ".agents", "skills", "review", "SKILL.md")
	if n := strings.Count(out.String(), agents); n != 1 {
		t.Errorf("%s reported %d times, want 1:\n%s", agents, n, out.String())
	}
	line := ""
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.Contains(l, agents) {
			line = l
		}
	}
	if !strings.Contains(line, "Antigravity") || !strings.Contains(line, "Codex") {
		t.Errorf("shared path does not name both tools: %q", line)
	}
	// Nothing was written to the home directory.
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills")); !os.IsNotExist(err) {
		t.Error("-project wrote into the home directory as well")
	}
}

// TestSkillInstallCodexHome checks Codex's home-directory location, which
// is ~/.agents/skills and not under ~/.codex.
func TestSkillInstallCodexHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0777); err != nil {
		t.Fatal(err)
	}

	out := captureOut(t)
	cmdSkill([]string{"-install"})
	if !strings.Contains(out.String(), "Codex") {
		t.Fatalf("Codex not reported:\n%s", out.String())
	}
	data, err := os.ReadFile(filepath.Join(home, ".agents", "skills", "review", "SKILL.md"))
	if err != nil {
		t.Fatalf("no Codex skill in ~/.agents/skills: %v", err)
	}
	if string(data) != skillText() {
		t.Error("Codex skill differs from the printed one")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "AGENTS.md")); !os.IsNotExist(err) {
		t.Error("wrote ~/.codex/AGENTS.md, want skills only")
	}
}

func TestSkillInstallToDir(t *testing.T) {
	dir := t.TempDir()
	out := captureOut(t)
	cmdSkill([]string{"-install", "-o", dir})

	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != skillText() {
		t.Error("installed skill differs from the printed one")
	}
	if !strings.Contains(out.String(), dir) {
		t.Errorf("did not report where it wrote:\n%s", out.String())
	}
}

// TestUpdateSkills checks the rule the server runs on at startup: bring an
// installed skill up to date, and never install one that is not there.
func TestUpdateSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Nothing installed: nothing to update, and nothing created.
	if wrote, _ := updateSkills(); len(wrote) != 0 {
		t.Errorf("updateSkills wrote %v into an empty home directory", wrote)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Errorf("wrote into an empty home directory: %v", entries)
	}

	// Install for Claude Code, then age the file as an older binary would
	// have left it.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0777); err != nil {
		t.Fatal(err)
	}
	captureOut(t)
	cmdSkill([]string{"-install"})
	skill := filepath.Join(home, ".claude", "skills", "review", "SKILL.md")

	// Up to date already: no write, so nothing is reported.
	if wrote, _ := updateSkills(); len(wrote) != 0 {
		t.Errorf("updateSkills rewrote an up-to-date skill: %v", wrote)
	}

	// What an older binary would have left: recognizable as review's own,
	// so it is brought up to date.
	const older = "stale instructions\n"
	defer func(saved []string) { knownSkills = saved }(knownSkills)
	sum := sha256.Sum256([]byte(older))
	knownSkills = append(knownSkills, hex.EncodeToString(sum[:]))

	if err := os.WriteFile(skill, []byte(older), 0666); err != nil {
		t.Fatal(err)
	}
	wrote, kept := updateSkills()
	if len(wrote) != 1 || wrote[0] != skill {
		t.Fatalf("updateSkills = %v, %v, want [%s] written", wrote, kept, skill)
	}
	data, err := os.ReadFile(skill)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != skillText() {
		t.Error("stale skill was not brought up to date")
	}

	// Something else under that name is somebody else's: left alone, and
	// said so rather than silently.
	const mine = "my own instructions\n"
	if err := os.WriteFile(skill, []byte(mine), 0666); err != nil {
		t.Fatal(err)
	}
	wrote, kept = updateSkills()
	if len(wrote) != 0 || len(kept) != 1 || kept[0] != skill {
		t.Fatalf("updateSkills = %v, %v, want [%s] left alone", wrote, kept, skill)
	}
	if data, err := os.ReadFile(skill); err != nil {
		t.Fatal(err)
	} else if string(data) != mine {
		t.Errorf("a skill review did not write was overwritten:\n%s", data)
	}

	// The tools that were never installed are still not installed: an
	// update is not a way to acquire instructions you did not ask for.
	for _, rel := range []string{".gemini", ".agents"} {
		if _, err := os.Stat(filepath.Join(home, rel)); !os.IsNotExist(err) {
			t.Errorf("updateSkills created %s for a tool that is not set up", rel)
		}
	}
}
