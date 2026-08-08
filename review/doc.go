// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
Review presents the pending commits of a git or jj repository for code
review in a web browser, in the style of Gerrit, and lets an agent read
and answer the resulting comments from the command line.

Usage:

	review [-a addr] [-n]
	review comments [-json] [-all] [-drafts] [-s n] [-c n] [change]
	review publish [change]
	review reply [-from name] [-resolve] thread [text]
	review resolve thread...
	review restart [-a addr]
	review skill [-print] [-install] [-project]
	review snapshot [-nopin] [change...]
	review snapshots [change]
	review stop
	review unresolve thread...

Flags follow the command they belong to, and every command takes -db to
name a different database file.

With no arguments, review makes sure a server is running, records a
snapshot of the repository holding the current directory, and opens a
browser at that repository's page. The first such run starts the server in
the background and prints where it is listening; later runs, in whatever
repository, find the running one and simply open another tab. The -a flag
sets the address to listen on (default localhost:2626); if it is busy,
review takes a free port instead. The -n flag prints the URL rather than
opening a browser.

	review stop
	review restart [-a addr]

stop and restart the background server. Restarting keeps the address it
was already on, so that open tabs keep working, unless -a says otherwise.

The server is one process per database, shared by every repository, and it
serves only localhost. Because a page in the browser could otherwise reach
it, requests are checked with Go's cross-origin protection: a state
changing request from another site is refused, while requests carrying no
Sec-Fetch-Site or Origin header, as from the command line, still pass.

# Reviewing

The main page lists the changes waiting in every repository that has been
reviewed, newest commit first, so that whatever was touched most recently
is at the top whichever repository it came from. Each repository also has
a page of its own at /name, where name is the last element of its path;
repositories that share a last element are told apart as name, name.1 and
so on, and a name once given is kept, so links do not move as other
repositories come and go. Visiting a repository's full path redirects to
its page, so a path pasted from a shell lands somewhere useful. Pressing u
walks back up: from a change to its repository, and from a repository to
the list of them all.

Which changes are pending depends on the version control system. In a jj
repository they are the mutable commits, which includes the working-copy
commit @, so uncommitted work shows up without any special handling. In a
git repository they are the commits not reachable from any remote ref,
plus a synthetic change holding the uncommitted working tree, so that the
two kinds of repository behave alike.

Clicking a change lists its files. The commit message is presented as the
first file, named /COMMIT_MSG, so that it can be reviewed and commented on
like any other. Against the parent commit it reads as a wholly new file,
since the parent's message belongs to a different change and is not an
earlier draft of this one; against an earlier snapshot it diffs normally,
which is how a reworded message shows up. Clicking a file shows a
side-by-side diff where clicking a line number leaves a comment on that
line.

An unpublished comment can be edited or deleted from the buttons in its
header. Published comments cannot: once a reply may have been written
against a comment, changing it would rewrite the conversation. Deleting
the only comment in a thread removes the thread with it.

Comments start as drafts, visible only in the web interface, until
published. This is Gerrit's model: it lets a review be composed as a whole
rather than dribbled out a remark at a time.

The diff colors are Gerrit's, taken from its source rather than
approximated, because they are chosen to stay legible for color-blind
readers. A changed line gets a pale background, and the part of the line
that actually changed gets a strong one. A chunk that only adds or only
removes lines has no intraline information to show, so those lines are
strongly colored throughout.

# Snapshots

Reviewing local commits means reviewing a moving target: answering a
comment usually means amending the commit it was written against. A
snapshot records the commit ID a change points at, the way Gerrit records
a patch set, and every comment belongs to the snapshot it was written
against.

	review snapshot

records a snapshot of every change; naming changes records only those. A
change is snapshotted automatically the first time it is viewed, so there
is always something for comments to attach to. Grabbing a snapshot when
the change has not moved is a reported no-op, so repeating the command is
harmless. Snapshots are numbered from 1 within each change.

Any two snapshots can be compared. The base selector at the top of a
change chooses what the diff is against: the commit's parent, which is
the whole change, or an earlier snapshot, which is just what has happened
since you last looked. That second view is the useful one after an agent
has been at work.

# Marking Things Reviewed

Each file in a change and each snapshot of it has a reviewed button, which
lights up solid green once pressed and stays lit. The button is the
indicator: there is nothing else to read.

Marking a snapshot reviewed also decides what later diffs are shown
against. Opening a file with no base chosen explicitly compares it against
the newest snapshot that has been marked reviewed, so you see only what has
changed since you last looked at it; with nothing marked yet, the base is
the commit's parent and you see the whole change. The base selector says
"showing changes since you last reviewed" when it has made this choice, and
picking Parent from it overrides the choice and shows the change entire.

File marks are per snapshot: marking a file reviewed in snapshot 2 says
nothing about snapshot 3, which is the point, since the file may have
changed. File marks do not affect the base; only snapshot marks do.

The LGTM button on a change records that its snapshot looks good. Like the
reviewed marks it belongs to the snapshot it was put on, so a new snapshot
arrives without it, and the change list shows the mark only when it is on
the newest snapshot. Both marks appear in the change list as chips.

Because an amended commit is no longer reachable, git is free to garbage
collect it and break an old snapshot. Grabbing a snapshot therefore also
writes a ref under refs/review to pin the commit. This is the only change
review makes to a repository; the -nopin flag disables it, at the cost of
snapshots that can rot. In a jj repository the ref goes into the git
store backing the repo, which works whether or not the repo is colocated.

# Comments and Changes That Move

A change is identified by something stabler than its commit hash: in jj,
the change ID, which is stable across rewrites by design; in git, the
Change-Id trailer if the commit has one. Failing both, the commit hash is
used, and comments will not survive an amend.

Comments additionally record the text of the line they were attached to.
When a comment written against an earlier snapshot is displayed on a later
one, review looks for that text near its old line and draws the comment
where it now lives, marked with the snapshot it came from. If the text is
gone entirely, the comment is listed at the top of the file and marked
stale rather than silently dropped.

# Command-Line Mode

The command-line mode exists so that an agent can take part in a review.

	review comments

prints the unresolved comment threads, each with its number, the file and
line it is attached to, the code it was written against, and the comments
themselves. Published comments only: drafts are the reviewer's unfinished
thoughts, and -drafts is required to see them. The -all flag includes
resolved threads, -s limits the output to one snapshot, and -c sets how
much context to show.

A comment belongs to the snapshot it was written against, which is often
older than the working tree, so its line has usually moved. Review finds
the line again by the text it was attached to and prints where it is now,
saying "written at line N" when it moved and "now gone" when the code has
been deleted.

The context shown under each comment is the snapshot's text, not the
working tree's, and is numbered by the snapshot's line numbers. The
heading says where to make the change; the context says what the comment
meant. Showing the current text there would put the reviewer's words
beside code they never saw, which is how a comment gets misread.

The -json form carries both: "current_line" and "line", with "context"
numbered by the latter and "stale" when the line has gone.

	review reply -resolve 12 'Fixed: renamed to parseHeader.'

replies to thread 12 and marks it resolved. Replies made here are always
recorded as not coming from the reviewer, so the web interface can draw
them differently, and are attributed to "agent" unless -from gives another
name. They are published immediately, since an agent has no publish step.
Omitting the text reads it from standard input.

The intended loop is that the agent reads the comments, fixes what it can,
replies and resolves the trivial ones, and leaves the rest; then the human
runs "review snapshot" and compares the new snapshot against the old one
to see exactly what changed.

# Telling an Agent How to Use This

	review skill -print

prints the instructions an agent needs: the loop above, what each field of
the JSON means, and the rules about what not to touch. It is enough to say
"address the review comments; run review skill -print for details".

	review skill -install

installs those instructions for the coding agents that are actually set up
on the machine, and leaves the others alone; a tool is taken to be in use
if its configuration directory exists under the home directory.

Claude Code, Antigravity, and Codex all load skills on demand and read the
same SKILL.md format, so they get the whole text:

	~/.claude/skills/review/SKILL.md
	~/.gemini/config/skills/review/SKILL.md
	~/.agents/skills/review/SKILL.md

The last of those is a shared convention rather than one tool's private
directory, so a single file can serve more than one agent; review writes
each path once and says which tools it serves. Nothing else is touched:
review installs skills, and never edits an instructions file that an agent
would read at the start of every session.

The -project flag writes the same things into the repository instead, so
that they travel with the code, at each tool's own workspace path: Claude
Code reads .claude/skills, while Antigravity and Codex both read
.agents/skills. The -o flag writes the skill to one named directory and
nowhere else.

An agent without a skill mechanism of its own is served by telling it to
run "review skill -print".

# Keyboard Shortcuts

The keyboard shortcuts are Gerrit's, so muscle memory carries over. Press
? for the full list. In brief: j and k move, o opens, u goes up, n and p
move between diff chunks, N and P between comment threads, c writes a
comment, and ⌘-Enter saves it. G grabs a snapshot.

Pressing n on the last chunk of a file parks the cursor on its last line
and shows a bar saying that pressing n again moves to the next file that
is not yet marked reviewed; p does the same backwards. A file that did not
change between the two revisions being compared has no chunks at all, so
the first press offers the next file immediately. Note that n never marks
anything reviewed, it only skips what is already marked; M is the shortcut
that marks the current file reviewed and moves on.

Moving to the next unreviewed file wraps around the file list, because
what is left unreviewed is often behind you: the commit message comes
first and is easy to leave for last. Only when nothing is left unreviewed
does the second press go up to the change, which makes reaching the change
page mean the work is done.

Gerrit's patch-set combo drives snapshots: v followed by s diffs against
the base, v w against the latest snapshot, and v b the base against the
latest.

# Storage

Comments live in a SQLite database at $HOME/.config/review/review.db, or
under $XDG_CONFIG_HOME if that is set. One database holds the reviews of
every repository on the machine, keyed by repository root, which is what
lets the main page show them all at once however it was started. The
command line always works on the repository containing the current
directory.
*/
package main
