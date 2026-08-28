// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// agentHelp is what an agent needs to know to take part in a review. It
// is the body of the skill that "review skill" prints and installs, so
// there is only ever one copy of these instructions.
const agentHelp = `The review command holds code review comments for the commits in this
repository. A human reviewer leaves comments in a web interface; you read
them from the command line, fix what they ask for, and reply.

The loop:

 1. Read the open comments:

        review comments

    Each thread is printed with its number in brackets, the file and line
    it is attached to as that file stands now, the code it was written
    against, and the comments themselves.

    Those two are deliberately different. Comments are written against a
    snapshot of the change, which is often older than the code in front of
    you, so the line has usually moved. The heading gives the line as it
    is now, which is where to make the change; the context below it is the
    text as the reviewer saw it, which is what the comment means. Reading
    a comment beside code the reviewer never saw is how you misread it.

    The heading says "written at line N" when the line has moved, and
    "now gone" when the code it was attached to has been deleted. In that
    case the context is the only record of what was meant, so read it
    carefully before deciding what the comment was about.

    Only published comments are listed. Drafts are the reviewer's
    unfinished thoughts: do not act on them, and do not pass -drafts.

    Text is usually the easier form to read. Pass -json if you would
    rather parse it: "current_line" is the line as it is now, "line" is
    the line it was written at, "context" is the snapshot's text numbered
    by that older line, and "stale" says the line has gone. Use -c to
    change how much context is shown.

 2. Fix what each thread asks for, editing the code as you normally would.
    Amending the commit is expected; comments survive it.

    In a jj repository, make the fix in a new commit on top of the one it
    belongs to, and fold it in once it is right:

        jj new K          # K is the change the comment is on
        ... edit files ...
        jj squash         # fold the fix into K

    Not "jj edit K". That makes K itself the working copy and rewrites it
    with every file you save, so a fix that turns out wrong has to be
    dug back out of the operation log. With "jj new" the fix sits in a
    commit of its own until you squash it, and abandoning it costs one
    command:

        jj abandon        # throw the fix away, K untouched

    Squashing rebases whatever is stacked above K, which was going to
    happen anyway, and K keeps its change ID, so its comments stay put.

    Work through the commits from the oldest to the newest, not in the
    order the comments happen to be printed in. Later commits are stacked
    on the ones before them, so a fix to an early commit is rebased
    through everything above it; fixing a later one first only means
    doing that work twice, and can leave conflicts to untangle in between.

 3. Reply to every thread you acted on, by its number:

        review reply -resolve 12 'Renamed to parseHeader.'

    -resolve closes the thread. Replies made from the command line are
    always marked as not coming from the reviewer, so the web interface
    can draw them apart from their own comments, and are recorded as from
    "agent" unless you give a name:

        review reply -from claude -resolve 12 'Renamed to parseHeader.'

    Resolve only what you actually fixed. If a comment asks for a judgement
    call, a design decision, or anything you are unsure about, reply
    without -resolve and say what you need:

        review reply 12 'Could go two ways here: ... which do you prefer?'

    Ask through the review, not in conversation. A question about a
    comment belongs in a reply on that thread, where the reviewer reads it
    beside the code and the comment it is about; asked in conversation it
    arrives with none of that. So do not stop and put the question to the
    user: leave the thread unresolved and go on to the next one. Waiting
    on an answer to one comment is no reason to leave the other twenty
    untouched, and by the time the answer comes everything else is done.

    Omitting the text reads the reply from standard input, which is easier
    for replies of more than a line.

 4. Once you have finished all the threads, record a snapshot, once:

        review snapshot

    That gives the reviewer a point to compare against, so they can see
    exactly what you changed since they commented. Do not snapshot after
    each individual fix.

 5. Say what you did, briefly: what you fixed and resolved, and which
    threads you left open and why. That is a summary of work already
    finished, not a question waiting on an answer.

Rules:

  - Never resolve a thread you did not address.
  - Never stop the work to ask about a comment. Reply on its thread,
    leave it unresolved, and carry on with the rest.
  - Never speak for the reviewer. Do not publish their drafts; "review
    publish" is theirs to run.
  - When a comment's line is gone, work out from the text it was written
    against what it referred to, and say in your reply what you concluded.
  - If you cannot work out what a comment refers to, reply asking, rather
    than guessing and resolving.

Other commands that may be useful:

    review comments CHANGE   comments on one change only
    review comments -all     include threads already resolved
    review snapshots         list the snapshots of each change

When you have been asked to review the code rather than to answer comments
on it, you can start a thread of your own, at the line it is about:

    review add CHANGE file.go:42 'This drops the error from Close.'

Use it only when reviewing is the task. While you are working through
someone else's comments, reply to them; a new thread there is a remark
addressed to nobody.
`

// skillFrontmatter makes the instructions discoverable to Claude Code,
// which reads skills from SKILL.md files and decides when one applies
// from its description.
const skillFrontmatter = `---
name: review
description: Read and answer local code review comments left with the "review" command. Use when the user asks to address review comments or review feedback, to look at unresolved comments on a change, or to reply to or resolve review threads. Also use when a task mentions the review tool, review threads, or snapshots of a change.
---

# Answering review comments

`

// skillText is the whole skill: what -print writes to standard output and
// what -install writes to a file, so the two cannot differ.
func skillText() string { return skillFrontmatter + agentHelp }

// An agentTool is a coding agent that review knows how to tell about
// itself. Marker is a directory under the home directory whose existence
// means the tool is set up for this user, so that -install only touches
// tools actually in use.
//
// Skill names the file to write the skill to, relative to the home
// directory, and ProjectSkill the same relative to a repository root, for
// instructions that travel with the code.
type agentTool struct {
	Name                string
	Marker              string
	Skill, ProjectSkill string
}

// Claude Code, Antigravity, and Codex all read the same SKILL.md format,
// so one skill text serves them all; only the directories differ. The
// .agents/skills path is a shared convention: Codex looks there in the
// home directory and in the repository, and Antigravity looks there in a
// workspace, so one file can serve more than one tool.
var agentTools = []agentTool{
	{
		Name:         "Claude Code",
		Marker:       ".claude",
		Skill:        filepath.Join(".claude", "skills", "review", "SKILL.md"),
		ProjectSkill: filepath.Join(".claude", "skills", "review", "SKILL.md"),
	},
	{
		// The Gemini CLI shares this directory but reads no skills, so a
		// user who has only that gets an inert file and should run
		// "review skill -print" instead.
		Name:         "Antigravity",
		Marker:       ".gemini",
		Skill:        filepath.Join(".gemini", "config", "skills", "review", "SKILL.md"),
		ProjectSkill: filepath.Join(".agents", "skills", "review", "SKILL.md"),
	},
	{
		// Codex looks in $HOME/.agents/skills, not under ~/.codex, which
		// holds only its configuration.
		Name:         "Codex",
		Marker:       ".codex",
		Skill:        filepath.Join(".agents", "skills", "review", "SKILL.md"),
		ProjectSkill: filepath.Join(".agents", "skills", "review", "SKILL.md"),
	},
}

func cmdSkill(args []string) {
	f := newFlags("skill", "[-print] [-install] [-project] [-o dir]")
	doPrint := f.Bool("print", false, "print the skill to standard output (the default)")
	doInstall := f.Bool("install", false, "install the skill for the agents set up on this machine")
	project := f.Bool("project", false, "with -install, write the skill into the repository rather than the home directory")
	outDir := f.String("o", "", "with -install, write the skill into `dir` and nowhere else")
	f.Parse(args)
	if f.NArg() != 0 {
		f.Usage()
	}
	if *doPrint && *doInstall {
		log.Fatal("cannot use both -print and -install")
	}
	if !*doInstall {
		fmt.Fprint(stdout, skillText())
		return
	}

	// An explicit destination overrides detection entirely.
	if *outDir != "" {
		path := filepath.Join(*outDir, "SKILL.md")
		if err := writeSkill(path); err != nil {
			log.Fatal(err)
		}
		fmt.Fprintf(stdout, "wrote %s\n", path)
		return
	}

	root, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	if *project {
		dir, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
		repo, err := OpenRepo(dir)
		if err != nil {
			log.Fatal(err)
		}
		root = repo.Root()
	}

	// Which tools are set up is always a question about the home
	// directory, even when the files are written into a repository.
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	// More than one tool can read the same file, so gather the work first
	// and write each path once, naming everything it serves.
	var order []string
	serves := map[string][]string{}
	// The relative path is what says whether a tool has a skill directory
	// at all: joining an empty one onto the root would name the root.
	add := func(rel, name string) {
		if rel == "" {
			return
		}
		path := filepath.Join(root, rel)
		if _, ok := serves[path]; !ok {
			order = append(order, path)
		}
		serves[path] = append(serves[path], name)
	}

	found := 0
	for _, t := range agentTools {
		if _, err := os.Stat(filepath.Join(home, t.Marker)); err != nil {
			continue
		}
		found++
		skill := t.Skill
		if *project {
			skill = t.ProjectSkill
		}
		add(skill, t.Name)
	}
	for _, path := range order {
		if err := writeSkill(path); err != nil {
			log.Fatal(err)
		}
		fmt.Fprintf(stdout, "%s: wrote %s\n", strings.Join(serves[path], ", "), path)
	}
	if found == 0 {
		fmt.Fprintf(stdout, "no coding agents found in %s\n", home)
		fmt.Fprintf(stdout, "Tell yours to run \"review skill -print\" for instructions.\n")
		return
	}
	fmt.Fprintf(stdout, "Ask an agent to address the review comments and it will find this.\n")
}

// knownSkills are the sha256 hashes of the skill texts review has
// written before. A file hashing to one of them is a copy review wrote
// that nobody has touched since, so bringing it up to date takes nothing
// away from anyone.
//
// When the text changes, the hash it had goes here, or the copies the
// last release installed stop being recognized as review's own.
var knownSkills = []string{
	"de80b5a33f6040d0f99c18c6998084e1a89106122b404edbbbb826b899241a94", // review/v0.0.1 through v0.0.6
}

// ourSkill reports whether a file found at a skill path is one review
// wrote and left alone: the text it writes now, or one it used to write.
// Anything else is somebody else's — a skill of their own under that
// name, or this one after they edited it — and is not review's to rewrite.
func ourSkill(data []byte) bool {
	if string(data) == skillText() {
		return true
	}
	sum := sha256.Sum256(data)
	return slices.Contains(knownSkills, hex.EncodeToString(sum[:]))
}

// updateSkills rewrites every already-installed copy of the skill that has
// fallen behind this binary, and reports the paths it wrote and the paths
// it found something else at and left alone.
//
// It never installs one where there is none. Putting instructions in front
// of an agent is the user's decision, made by running "review skill
// -install"; keeping them current afterwards is not a decision but
// bookkeeping, and forgetting it is how an agent ends up reading about
// flags this binary no longer has.
//
// Nor does it write over anything it did not write. Updating is only
// bookkeeping while the file is review's own; a skill somebody wrote for
// themselves under that name, or a copy of this one they have since
// edited, is theirs, and the most this does about one is say it is there.
//
// Only the copies under the home directory are considered. A -project
// install lives in a repository the server may know nothing about, and
// belongs to that repository's checkout rather than to whoever happens to
// be serving it.
func updateSkills() (wrote, kept []string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil
	}
	want := skillText()
	seen := map[string]bool{}
	for _, t := range agentTools {
		if t.Skill == "" {
			continue
		}
		path := filepath.Join(home, t.Skill)
		if seen[path] {
			continue
		}
		seen[path] = true
		// A file that is not there is not installed, and one that already
		// matches needs no write: the common case touches no disk at all.
		data, err := os.ReadFile(path)
		if err != nil || string(data) == want {
			continue
		}
		if !ourSkill(data) {
			kept = append(kept, path)
			continue
		}
		if err := writeSkill(path); err != nil {
			log.Printf("updating skill at %s: %v", path, err)
			continue
		}
		wrote = append(wrote, path)
	}
	return wrote, kept
}

func writeSkill(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(skillText()), 0666)
}
