// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "encoding/json"

// The keyboard shortcuts are Gerrit's, taken from
// polygerrit-ui/app/services/shortcuts/shortcuts-config.ts, minus the
// bindings that only mean something on a Gerrit server: starring,
// attention sets, topics, reviewers, downloads, blame, and the pages
// for repositories and groups.
//
// This table is the only copy. The help dialog is rendered from it, and
// it is also serialized into each page for keys.js to dispatch on, so
// the two cannot drift apart.

// A keyBinding is one keyboard shortcut.
//
// Keys are written the way KeyboardEvent.key reports them, so "j" and "J"
// are distinct. A space separates the two halves of a combo, as in "g i".
// "Mod" means Control or Command.
type keyBinding struct {
	Keys   []string `json:"keys"`
	Action string   `json:"action"`
	Text   string   `json:"text"`
	Repeat bool     `json:"repeat,omitempty"`
}

// A keySection is a group of bindings that applies to certain pages.
// Gerrit deliberately reuses keys across contexts: r is "toggle reviewed"
// in the file list and "mark reviewed" on a diff, ] and [ move between
// files on a diff but jump to the first and last file in the file list.
type keySection struct {
	Title    string       `json:"title"`
	Pages    []string     `json:"pages"`
	Bindings []keyBinding `json:"bindings"`
}

var keyHelp = []keySection{{
	Title: "Global shortcuts",
	Pages: []string{"all", "changes", "files", "diff"},
	Bindings: []keyBinding{
		{Keys: []string{"?"}, Action: "showHelp", Text: "Show this dialog"},
		{Keys: []string{"/"}, Action: "focusSearch", Text: "Search"},
		{Keys: []string{"g i"}, Action: "goRepos", Text: "Go to every repository"},
	},
}, {
	Title: "All repositories",
	Pages: []string{"all"},
	Bindings: []keyBinding{
		{Keys: []string{"j"}, Action: "nextItem", Text: "Select next change", Repeat: true},
		{Keys: []string{"k"}, Action: "prevItem", Text: "Select previous change", Repeat: true},
		{Keys: []string{"o", "Enter"}, Action: "openItem", Text: "Show selected change"},
		{Keys: []string{"R"}, Action: "reload", Text: "Refresh the list"},
	},
}, {
	Title: "Change list",
	Pages: []string{"changes"},
	Bindings: []keyBinding{
		{Keys: []string{"j"}, Action: "nextItem", Text: "Select next change", Repeat: true},
		{Keys: []string{"k"}, Action: "prevItem", Text: "Select previous change", Repeat: true},
		{Keys: []string{"o", "Enter"}, Action: "openItem", Text: "Show selected change"},
		{Keys: []string{"x"}, Action: "toggleCheckbox", Text: "Toggle checkbox"},
		{Keys: []string{"R"}, Action: "reload", Text: "Refresh list of changes"},
		{Keys: []string{"G"}, Action: "grabSnapshot", Text: "Grab a snapshot of the selected change"},
		{Keys: []string{"u"}, Action: "goRepos", Text: "Up to every repository"},
	},
}, {
	Title: "File list",
	Pages: []string{"files"},
	Bindings: []keyBinding{
		{Keys: []string{"j", "ArrowDown"}, Action: "nextItem", Text: "Select next file", Repeat: true},
		{Keys: []string{"k", "ArrowUp"}, Action: "prevItem", Text: "Select previous file", Repeat: true},
		{Keys: []string{"o", "Enter"}, Action: "openItem", Text: "Go to selected file"},
		{Keys: []string{"i"}, Action: "toggleInlineDiff", Text: "Show/hide selected inline diff"},
		{Keys: []string{"I"}, Action: "toggleAllInlineDiffs", Text: "Show/hide all inline diffs"},
		{Keys: []string{"r"}, Action: "toggleReviewed", Text: "Toggle review flag on selected file"},
		{Keys: []string{"]"}, Action: "openFirstFile", Text: "Go to first file"},
		{Keys: []string{"["}, Action: "openLastFile", Text: "Go to last file"},
		{Keys: []string{"x"}, Action: "expandAllThreads", Text: "Expand all comment threads"},
		{Keys: []string{"z"}, Action: "collapseAllThreads", Text: "Collapse all comment threads"},
		{Keys: []string{"a"}, Action: "openPublish", Text: "Publish drafts"},
		{Keys: []string{"P"}, Action: "openPublish", Text: "Publish drafts"},
		{Keys: []string{"R"}, Action: "reloadLatest", Text: "Reload the change at the latest snapshot"},
		{Keys: []string{"M"}, Action: "reviewChange", Text: "Mark the change reviewed and go up to the change list"},
		{Keys: []string{"u"}, Action: "goChanges", Text: "Up to this repository's change list"},
	},
}, {
	Title: "Diffs",
	Pages: []string{"diff"},
	Bindings: []keyBinding{
		{Keys: []string{"j", "ArrowDown"}, Action: "nextItem", Text: "Go to next line", Repeat: true},
		{Keys: []string{"k", "ArrowUp"}, Action: "prevItem", Text: "Go to previous line", Repeat: true},
		{Keys: []string{"n"}, Action: "nextChunk", Text: "Go to next diff chunk"},
		{Keys: []string{"p"}, Action: "prevChunk", Text: "Go to previous diff chunk"},
		{Keys: []string{"N"}, Action: "nextThread", Text: "Go to next comment thread"},
		{Keys: []string{"P"}, Action: "prevThread", Text: "Go to previous comment thread"},
		{Keys: []string{"."}, Action: "visibleLine", Text: "Move cursor to currently visible code"},
		{Keys: []string{"c"}, Action: "newComment", Text: "Draft new comment"},
		{Keys: []string{"Mod+Enter", "Mod+s"}, Action: "saveComment", Text: "Save comment"},
		{Keys: []string{"e"}, Action: "expandAllThreads", Text: "Expand all comment threads"},
		{Keys: []string{"E"}, Action: "collapseAllThreads", Text: "Collapse all comment threads"},
		{Keys: []string{"h"}, Action: "toggleThreads", Text: "Hide/display all comment threads"},
		{Keys: []string{"X"}, Action: "toggleAllContext", Text: "Toggle all diff context"},
		{Keys: []string{"A"}, Action: "toggleLeftPane", Text: "Hide/show left diff"},
		{Keys: []string{"Shift+ArrowLeft"}, Action: "leftPane", Text: "Select left pane"},
		{Keys: []string{"Shift+ArrowRight"}, Action: "rightPane", Text: "Select right pane"},
		{Keys: []string{"m"}, Action: "toggleUnified", Text: "Toggle unified/side-by-side diff"},
		{Keys: []string{"r"}, Action: "toggleReviewed", Text: "Mark/unmark file as reviewed"},
		{Keys: []string{"M"}, Action: "reviewedNextFile", Text: "Mark file as reviewed and go to next unreviewed file"},
		{Keys: []string{"f"}, Action: "openFileList", Text: "Open file list"},
		{Keys: []string{"]"}, Action: "nextFile", Text: "Go to next file"},
		{Keys: []string{"["}, Action: "prevFile", Text: "Go to previous file"},
		{Keys: []string{"J"}, Action: "nextFileWithComments", Text: "Go to next file that has comments"},
		{Keys: []string{"K"}, Action: "prevFileWithComments", Text: "Go to previous file that has comments"},
		{Keys: []string{","}, Action: "diffPrefs", Text: "Show diff preferences"},
		{Keys: []string{"u"}, Action: "openFileList", Text: "Up to the change"},
	},
}, {
	// Gerrit's patch-set comparison combo, which maps one to one onto
	// snapshots: v then a second key chooses what to diff against.
	Title: "Snapshots",
	Pages: []string{"files", "diff"},
	Bindings: []keyBinding{
		{Keys: []string{"G"}, Action: "grabSnapshot", Text: "Grab a snapshot"},
		{Keys: []string{"v s", "v ArrowDown"}, Action: "diffAgainstBase", Text: "Diff against base"},
		{Keys: []string{"v w", "v ArrowUp"}, Action: "diffAgainstLatest", Text: "Diff against latest snapshot"},
		{Keys: []string{"v a", "v ArrowLeft"}, Action: "diffBaseAgainstLeft", Text: "Diff base against left"},
		{Keys: []string{"v d", "v ArrowRight"}, Action: "diffRightAgainstLatest", Text: "Diff right against latest"},
		{Keys: []string{"v b"}, Action: "diffBaseAgainstLatest", Text: "Diff base against latest"},
	},
}}

// keyMapJSON is the shortcut table as JSON, embedded in every page so that
// keys.js dispatches from the same table the help dialog is rendered from.
func keyMapJSON() string {
	data, err := json.Marshal(keyHelp)
	if err != nil {
		// The table is a constant; marshaling it cannot fail in practice.
		panic(err)
	}
	return string(data)
}

// keyActions returns every action name used in the table.
func keyActions() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range keyHelp {
		for _, b := range s.Bindings {
			if !seen[b.Action] {
				seen[b.Action] = true
				out = append(out, b.Action)
			}
		}
	}
	return out
}
