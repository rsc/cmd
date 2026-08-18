// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
Review presents the pending commits of a git or jj repository for code
review in a web browser, in the style of Gerrit, and lets an agent read
and answer the resulting comments from the command line.

Usage:

	review [-a addr] [-n]
	review add [-from name] [-s n] change file[:line] [text]
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
reviewed, most recently snapshotted first, so that whatever was touched
most recently is at the top whichever repository it came from. That is the
time the lists show, too: a commit's own date is when the work was first
written, which a rebase does not move, and which says nothing about when
the change last reached the reviewer.

Each repository also has a page of its own at /name, where name is the
last element of its path; repositories that share a last element are told
apart as name, name.1 and so on, and a name once given is kept, so links
do not move as other repositories come and go. Visiting a repository's
full path redirects to its page, so a path pasted from a shell lands
somewhere useful. Pressing u walks back up: from a change to its
repository, and from a repository to the list of them all.

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

Each file in the list carries the size of the diff the row opens, as +N
−M. The counts leave out the lines a rebase brought, for the same reason
the diff mutes them: they are not this change's work, and counting them
would put a four-figure number beside a file the change never touched.
A file wholly inherited that way reads +0 −0, which is the whole of what
there is to say about it.

An unpublished comment can be edited or deleted from the buttons in its
header. Published comments cannot: once a reply may have been written
against a comment, changing it would rewrite the conversation. Deleting
the only comment in a thread removes the thread with it.

Comments start as drafts, visible only in the web interface, until
published. This is Gerrit's model: it lets a review be composed as a whole
rather than dribbled out a remark at a time.

A change publishes its own drafts from the button on its page, or the a
key, as in Gerrit. The repository's page has a button that publishes every
draft in the repository, across all of its changes, for a review written a
commit at a time up a stack; it says how many it will publish and is not
there when there are none. That one has no key of its own on purpose: it
cannot be undone, it reaches past what is on screen, and a is one finger
away from the keys that move around that page.

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
the change has not moved records nothing and says so, so repeating the
command is harmless. It is not quite a no-op: it still settles the
reviewed marks, which is what to reach for when they look out of date.
Snapshots are numbered from 1 within each change.

Any two snapshots can be compared. The base selector at the top of a
change chooses what the diff is against: the commit's parent, which is
the whole change, or an earlier snapshot, which is just what has happened
since you last looked. That second view is the useful one after an agent
has been at work.

# Rebased Changes

Commits are stacked, so editing one moves every commit above it onto the
new version. The commits above did not change, but their snapshots did,
and the edit made below turns up in each of their snapshot-to-snapshot
diffs. Gerrit calls such a region "due to rebase" and paints it in muted
colors; review does the same, using Gerrit's palette for that too:
lavender where a change of its own would be green, tan where it would be
red. A note above the diff says what the muted colors mean.

A line is the change's own work when the change put it there: when it is
one of the lines an edit from the commit underneath produces. Everything
else in the file came up from below, so a difference in it between the two
snapshots is a difference the rebase brought. Each side is asked about its
own parent, so the question is always about lines that really exist rather
than about matching one diff's text against another's — which means that
neither an edit of the change's own sitting in the middle of a rebased
passage nor a line repeated elsewhere in the file can mislead it.

The two sides are answered separately, because they can differ. A line the
change used to write itself can be replaced by one that now comes from
below, and then the red half of that row is the change's own work and the
green half is not.

A file is called rebase-only when every changed line in it is inherited.
Most are settled by two file listings: a file the change touches in
neither snapshot can only be showing what a rebase brought. But a file
the change does edit can be wholly inherited too, when its own edit is
the same on both sides and only the ground underneath it moved, which is
the common case when a rebase sweeps a whole tree. Answering that means
reading the file, so it is asked only where it can be true — of the files
the change edits that the rebase touched as well — and the answer is
remembered, since a snapshot never moves.

Gerrit stops there: its file list has no idea which files are which, so
the only way to learn that a file is nothing but rebase is to open it and
find every line muted. Review carries the distinction up to the file list
and into the keyboard, where it saves the trip. A file the change does not
touch on either side is left out of the list altogether, with a link at
the foot of it offering to show the ones held back; M passes over such a
file on its way to the next unreviewed one, and n and p step over
inherited chunks inside a file the same way they step over unchanged
text. What is left in front of you is the change's own work.

A comment keeps a file in the list whatever put the lines there. Somebody
having written something about a file is a better reason to look at it
than the diff being inherited is a reason not to, and a comment nobody
can find is worse than a file nobody needed to open.

The same goes for a file with no diff at all. Comparing against the
snapshot last marked reviewed narrows the list to what has moved since,
which is the point of doing it, but a comment written on anything else
would then have nowhere to be listed. Such a file is added to the end of
the list with no status letter, since the change did nothing to it, and
opening it shows the file and the comment on it. A comment on a file that
is no longer there at all keeps its place in the change's comment history
instead, there being no row left to click.

Collapsing the unchanged parts of a diff leaves the commented lines
showing, wherever they fall. A comment can sit a long way from anything
the change did, and a comment folded away behind an expander is one
nobody will answer.

The hidden rows are rendered all the same, so showing them is instant and
needs no round trip, and asking for one of them by name — coming back up
from it with u — shows them without being asked. Shown, they read "rebase
only" where another file reads "mark reviewed".

The commit message file has the same problem in miniature: its header
names the parent commit, and a rebase changes that line every time,
whatever it was that moved. So the header names the parent twice where it
can, by its commit ID and by its stable identity — the parent's jj change
ID, or its Change-Id trailer:

	JJ Parent:  zpzyxumyszvpxqkutowoqlywtyztvkqn
	Git Parent: 33e19a79562e

The git line changes on every rebase and says nothing. The line above it
does not, so when it does change, the change really has moved to a
different parent, which is worth knowing and easy to miss in a line that
always changes. A parent with no identity beyond its hash, as in a git
repository not using Change-Id trailers, is named once as before.

# Marking Things Reviewed

Each file in a change and each snapshot of it has a reviewed button, which
lights up solid green once pressed and stays lit. The button is the
indicator: there is nothing else to read.

A file is in one of three states, not two. Unreviewed and reviewed are
verdicts, recorded when the button is pressed. The third, "rebase only",
is not a verdict but a fact about the diff: the change does not touch the
file, so there is nothing in it to have an opinion about. It is worked out
afresh for whatever two snapshots are being compared rather than stored,
because it is true of a comparison and not of a file, and a verdict beats
it — a file someone has read reads as reviewed whatever put the changes
there. Pressing the button still marks such a file reviewed, for anyone
who wants the whole list green.

Marking a snapshot reviewed marks every file it changes, and unmarking it
takes them back. Saying a snapshot has been read is saying its files have,
and a snapshot lit up over a list of unlit files would be a contradiction
on one screen. A mark that could be set as a group but only taken back one
file at a time would be a trap, so the pair of them go together.

Marking a snapshot reviewed also decides what later diffs are shown
against. Opening a file with no base chosen explicitly compares it against
the newest snapshot that has been marked reviewed, so you see only what has
changed since you last looked at it; with nothing marked yet, the base is
the commit's parent and you see the whole change. The base selector says
"showing changes since you last reviewed" when it has made this choice, and
picking Parent from it overrides the choice and shows the change entire.
On a file the change does not touch at all it says instead that the changes
in the file are entirely due to earlier commits, which is the more useful
thing to know: there is nothing there to review.

File marks are per snapshot: marking a file reviewed in snapshot 2 says
nothing about snapshot 3, which is the point, since the file may have
changed. A file that did not change carries its mark forward, though, so
that a new snapshot touching one file does not ask for the whole change
to be read again. The commit message carries too when a rebase has only
moved the line naming its parent, which is not a word of the message
changing. File marks do not affect the base; only snapshot marks do.

A snapshot holding nothing but a rebase inherits both marks from the one
before it. Editing a commit low in a stack rewrites every commit above it,
and would otherwise strip the lot, not one of which changed anything:
you would be asked to read a diff whose every line was somebody else's
work. So when everything separating two snapshots is a file the change
does not touch, and the commit message differs only in the line naming the
parent commit — the line a rebase always moves — the marks carry. What
looked good is still there, so the sign-off travels with the reviewed
mark. A file the change edits counts as separating them only if its own
edit to that file changed: the same test the file list uses to call a
file rebase-only decides it, so what the reader is told and what this
concludes cannot drift apart. Finding a snapshot that does hold work of
its own is written down, since neither snapshot can move and the answer
would otherwise be worked out again on every grab.
Anything the change did itself stops the carry, and so does anything the
repository will not answer for.

The LGTM button on a change records that its snapshot looks good. Like the
reviewed marks it belongs to the snapshot it was put on, so a new snapshot
arrives without it, and the change list shows the mark only when it is on
the newest snapshot.

Pressing it marks the snapshot and its files reviewed as well: saying a
snapshot looks good says it has been read. Taking the LGTM back does not
take that back — the reading still happened, and the reviewed marks are a
record of what has been looked at rather than of what was thought of it.
Because one implies the other, the change list shows the LGTM chip in
place of the reviewed chip rather than beside it, which would say the same
thing twice on every row.

Which snapshot carries it is shown in the change's own snapshot list, as a
chip on that snapshot's line. The mark belongs to a snapshot rather than to
the change, so where it sits is the whole of what it says: the same button
at the top of the page speaks only for the snapshot being viewed.

Because an amended commit is no longer reachable, git is free to garbage
collect it and break an old snapshot. Grabbing a snapshot therefore also
writes a ref under refs/reviewed to pin the commit. This is the only change
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

Better than re-anchoring a comment is showing it where it fits. So the
file:line link on a comment in a change's history opens the snapshot the
comment was written against on the left and the newest one on the right.
Following an old comment is asking what has become of the line since, and
that view answers it: the comment sits on the left beside the text it was
written about, with whatever replaced it alongside. A comment on the
newest snapshot has nothing to compare against and links to it plainly,
and one written on the parent side keeps its own link, being a remark
about the commit underneath, which a diff between two snapshots does not
show.

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

	review add I8d4c2f header.go:42 'This drops the error from Close.'

starts a thread instead of answering one, which is what the reviewer does
by clicking a line number in the web interface. It is for an agent asked
to review the code rather than to answer comments on it. The line number
may be left off to comment on the file as a whole, and the file may be
/COMMIT_MSG. The comment lands on the newest snapshot unless -s names an
earlier one, and like a reply it is published at once and attributed to
"agent" unless -from says otherwise.

Any file in the snapshot can be named, not only the ones the change
touches. A file it does not touch still turns up when one snapshot is
compared against another, carried in by a rebase, and asking why it moved
is a fair question to ask.

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

Starting the server rewrites any of those three files that is already
there and no longer matches the running binary, and says so. Installing
is a decision, made once; keeping the copy current afterwards is not, and
forgetting it leaves an agent reading about commands the binary no longer
has. A file that is not there stays not there, so this never installs
instructions nobody asked for, and one that already matches is left alone
rather than rewritten.

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

Both n and p bring the chunk they land on up to about ten lines below the
top of the window, so that what follows it is on screen: a chunk read with
its consequences off the bottom edge is half a chunk. Near the end of a
file they stop once the bottom of it is in view, since there is nothing
past that to scroll to, and a chunk already near the top is left where it
is rather than nudged. Neither stops on a chunk a rebase brought along;
those are stepped over like unchanged text, so that a file whose changes
are all inherited has nowhere for the cursor to stop.

Stepping off either end of the file list with ] or [ goes up to the
change, as it does in Gerrit, whose diff view answers a step past the end
with "up". Those two move once and say nothing; n and p are the pair that
stop to offer something first.

Pressing n on the last chunk of a file parks the cursor on its last line
and shows a bar saying that pressing n again moves to the next file that
is not yet marked reviewed; p does the same backwards. A file that did not
change between the two revisions being compared has no chunks at all, so
the first press offers the next file immediately. Note that n never marks
anything reviewed, it only skips what is already marked; M is the shortcut
that marks the current file reviewed and moves on.

Moving to the next unreviewed file wraps around the file list, because
what is left unreviewed is often behind you: the commit message comes
first and is easy to leave for last. Files in the rebase-only state are
passed over along with the reviewed ones, so the wrap ends at something
worth reading. Only when nothing is left unreviewed does the second press
go up to the change, which makes reaching the change page mean the work
is done.

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
