// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeFile writes data to a new file in a temporary directory
// and returns the file's name.
func writeFile(t *testing.T, name, data string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(file, []byte(data), 0666); err != nil {
		t.Fatal(err)
	}
	return file
}

// transcript is a small Claude Code transcript exercising the record types
// agentweb knows about: a plain user prompt, an assistant turn holding
// thinking, prose, and a tool call, the tool result that arrives in a
// following user record, and a final assistant record continuing the turn.
const transcript = `
{"type":"user","uuid":"u1","timestamp":"2026-01-02T03:04:05Z","cwd":"/tmp/x","message":{"role":"user","content":"hello <system-reminder>ignore me</system-reminder>there"}}
{"type":"assistant","uuid":"a1","timestamp":"2026-01-02T03:04:06Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"pondering"},{"type":"text","text":"Sure."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls","description":"list"}}]}}
{"type":"user","uuid":"u2","timestamp":"2026-01-02T03:04:07Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a\nb\n"}]}}
{"type":"assistant","uuid":"a2","timestamp":"2026-01-02T03:04:08Z","message":{"role":"assistant","content":[{"type":"text","text":"Two files."}]}}
{"type":"user","uuid":"u3","timestamp":"2026-01-02T03:04:09Z","isMeta":true,"message":{"role":"user","content":"meta record, skipped"}}
{"type":"ai-title","aiTitle":"Listing files"}
`

func TestClaudeMessages(t *testing.T) {
	msgs, err := claudeMessages(writeFile(t, "c.jsonl", transcript))
	if err != nil {
		t.Fatal(err)
	}
	// The tool result and the meta record add no messages of their own,
	// so the two assistant records merge into a single turn.
	if len(msgs) != 2 {
		t.Fatalf("claudeMessages returned %d messages, want 2", len(msgs))
	}

	user := msgs[0]
	if user.Role != "user" || len(user.Parts) != 1 {
		t.Fatalf("first message = %+v, want one user part", user)
	}
	if want := "hello there"; user.Parts[0].Text != want {
		t.Errorf("user text = %q, want %q", user.Parts[0].Text, want)
	}

	asst := msgs[1]
	if asst.Role != "assistant" || len(asst.Parts) != 4 {
		t.Fatalf("second message = %+v, want four assistant parts", asst)
	}
	kinds := []Kind{KindThinking, KindText, KindTool, KindText}
	for i, want := range kinds {
		if got := asst.Parts[i].Kind; got != want {
			t.Errorf("part %d kind = %v, want %v", i, got, want)
		}
	}

	tool := asst.Parts[2]
	if tool.Tool != "Bash" || tool.Result != "a\nb\n" || tool.IsError {
		t.Errorf("tool part = %+v, want Bash with result", tool)
	}
	// The most descriptive argument comes first, and summarizes the call.
	if len(tool.Args) != 2 || tool.Args[0] != (Arg{"command", "ls"}) || tool.Args[1] != (Arg{"description", "list"}) {
		t.Errorf("tool args = %+v, want command then description", tool.Args)
	}
	if got, want := tool.Brief(), "ls"; got != want {
		t.Errorf("tool brief = %q, want %q", got, want)
	}
}

func TestClaudeConv(t *testing.T) {
	file := writeFile(t, "c332bf5d.jsonl", transcript)
	c, err := claudeConv(file)
	if err != nil {
		t.Fatal(err)
	}
	want := Conv{
		ID:    "c332bf5d",
		Title: "Listing files",
		Dir:   "/tmp/x",
		Time:  time.Date(2026, 1, 2, 3, 4, 9, 0, time.UTC),
		File:  file,
	}
	if *c != want {
		t.Errorf("claudeConv = %+v, want %+v", *c, want)
	}
}

func TestClaudeConvTitles(t *testing.T) {
	// A custom title outranks the title the model chose,
	// and the last of each kind of title record wins.
	const titles = transcript + `
{"type":"custom-title","customTitle":"first"}
{"type":"ai-title","aiTitle":"later guess"}
{"type":"custom-title","customTitle":"last"}
`
	c, err := claudeConv(writeFile(t, "c.jsonl", titles))
	if err != nil {
		t.Fatal(err)
	}
	if c.Title != "last" {
		t.Errorf("title = %q, want %q", c.Title, "last")
	}

	// With no title records at all, the first user prompt stands in.
	const untitled = `
{"type":"user","uuid":"u1","cwd":"/tmp/y","message":{"role":"user","content":"why is the sky blue?\nand other questions"}}
`
	c, err = claudeConv(writeFile(t, "d.jsonl", untitled))
	if err != nil {
		t.Fatal(err)
	}
	if want := "why is the sky blue? ..."; c.Title != want {
		t.Errorf("title = %q, want %q", c.Title, want)
	}
	if c.Dir != "/tmp/y" {
		t.Errorf("dir = %q, want %q", c.Dir, "/tmp/y")
	}
}

func TestLookup(t *testing.T) {
	convs := []*Conv{
		{ID: "c332bf5d-5c2c-438e", Title: "Coordinator SCP support issue"},
		{ID: "c3390000-0000-0000", Title: "mote close error message"},
		{ID: "0bf67162-5d37-4fc1", Title: "implement mote remote cmd"},
	}
	var tests = []struct {
		arg  string
		want string // the ID found, or "" if the lookup should fail
	}{
		{"c332bf5d-5c2c-438e", "c332bf5d-5c2c-438e"}, // the whole ID
		{"0", "0bf67162-5d37-4fc1"},                  // one character is enough when it is unique
		{"c332", "c332bf5d-5c2c-438e"},               // a prefix of one of the two c3 IDs
		{"c339", "c3390000-0000-0000"},               // and of the other
		{"Coordinator", "c332bf5d-5c2c-438e"},        // title text
		{"COORDINATOR", "c332bf5d-5c2c-438e"},        // matched without regard to case
		{"c3", ""},                                   // prefix of two IDs
		{"mote", ""},                                 // in two titles
		{"c332bf5d-ffff", ""},                        // prefix of nothing
		{"", ""},                                     // matches every title, so no one conversation
	}
	for _, tt := range tests {
		c, err := lookup(convs, tt.arg)
		if tt.want == "" {
			if err == nil {
				t.Errorf("lookup(%q) = %q, want an error", tt.arg, c.ID)
			}
			continue
		}
		if err != nil {
			t.Errorf("lookup(%q): %v", tt.arg, err)
		} else if c.ID != tt.want {
			t.Errorf("lookup(%q) = %q, want %q", tt.arg, c.ID, tt.want)
		}
	}

	// An ambiguous prefix says so, and names what it found.
	_, err := lookup(convs, "c3")
	if err == nil || !strings.Contains(err.Error(), "2 conversations have IDs starting with") ||
		!strings.Contains(err.Error(), "c3390000-0000-0000") {
		t.Errorf("lookup(%q) error = %v, want both IDs listed", "c3", err)
	}
}

func TestOutputFile(t *testing.T) {
	c := &Conv{ID: "c332bf5d"}
	dir := t.TempDir()
	var tests = []struct{ out, want string }{
		{"", "c332bf5d.html"},
		{"talk.html", "talk.html"},
		{filepath.Join(dir, "new.html"), filepath.Join(dir, "new.html")},
		{dir, filepath.Join(dir, "c332bf5d.html")}, // an existing directory
	}
	for _, tt := range tests {
		if got := outputFile(tt.out, c); got != tt.want {
			t.Errorf("outputFile(%q) = %q, want %q", tt.out, got, tt.want)
		}
	}
}

func TestGroups(t *testing.T) {
	tool := func(name string) *Part { return &Part{Kind: KindTool, Tool: name} }
	text := &Part{Kind: KindText, Text: "prose"}
	think := &Part{Kind: KindThinking, Text: "pondering"}

	var tests = []struct {
		name  string
		parts []*Part
		want  []string // one entry per group: a label, or the kind of a lone part
	}{
		{"nothing to gather", []*Part{text}, []string{"text"}},
		{"a run of one is gathered too", []*Part{text, tool("Bash"), text},
			[]string{"text", "Ran tools (1 Bash)", "text"}},
		{"a run of two", []*Part{tool("Bash"), tool("Bash")}, []string{"Ran tools (2 Bash)"}},
		{"counted by tool, in first use order", []*Part{
			tool("Bash"), tool("WebFetch"), tool("Bash"), tool("Bash"),
		}, []string{"Ran tools (3 Bash, 1 WebFetch)"}},
		{"thinking ends a run", []*Part{
			tool("Bash"), tool("Bash"), think, tool("Read"), tool("Read"),
		}, []string{"Ran tools (2 Bash)", "thinking", "Ran tools (2 Read)"}},
		{"a run between prose", []*Part{
			text, tool("Bash"), tool("Read"), text,
		}, []string{"text", "Ran tools (1 Bash, 1 Read)", "text"}},
	}
	for _, tt := range tests {
		m := &Msg{Role: "assistant", Parts: tt.parts}
		var got []string
		for _, g := range m.Groups() {
			if g.Tools != nil {
				got = append(got, g.Label())
			} else {
				got = append(got, g.Part.KindName())
			}
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s: Groups = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestOnlySaid(t *testing.T) {
	msgs, err := claudeMessages(writeFile(t, "c.jsonl", transcript))
	if err != nil {
		t.Fatal(err)
	}
	// The assistant turn keeps its two pieces of prose but loses the
	// thinking and the tool call between them.
	said := onlySaid(msgs)
	if len(said) != 2 {
		t.Fatalf("onlySaid returned %d messages, want 2", len(said))
	}
	for _, m := range said {
		for _, p := range m.Parts {
			if p.Kind == KindThinking || p.Kind == KindTool {
				t.Errorf("%s message still has a %v part", m.Role, p.Kind)
			}
		}
	}
	if got := len(said[1].Parts); got != 2 {
		t.Errorf("assistant message has %d parts, want 2", got)
	}
	// The original is left alone, so -a still has everything to show.
	if got := len(msgs[1].Parts); got != 4 {
		t.Errorf("onlySaid changed the messages it was given: %d parts, want 4", got)
	}

	// A turn that was nothing but tool calls leaves no empty message behind.
	tools := []*Msg{{Role: "assistant", Parts: []*Part{{Kind: KindTool, Tool: "Bash"}}}}
	if got := onlySaid(tools); len(got) != 0 {
		t.Errorf("onlySaid of a tool-only turn returned %d messages, want 0", len(got))
	}
}

// TestClaudeHarnessText checks the text the harness writes into the user's
// messages. Left alone, the Markdown passes these tags through as HTML and
// the browser drops them, so that a finished background task reads as the
// user having said "b8a5efc1 toolu_018Eky /tmp/x.output completed".
func TestClaudeHarnessText(t *testing.T) {
	var tests = []struct {
		name string
		text string
		want []*Part
	}{
		{"a finished background task, summarized",
			"<task-notification>\n" +
				"<task-id>bqumrax6p</task-id>\n" +
				"<tool-use-id>toolu_018EkyZjGJS13wfY1eMmJXer</tool-use-id>\n" +
				"<output-file>/tmp/tasks/bqumrax6p.output</output-file>\n" +
				"<status>completed</status>\n" +
				`<summary>Background command "A/B stress" completed (exit code 0)</summary>` + "\n" +
				"</task-notification>",
			[]*Part{{Kind: KindNote, Text: `Background command "A/B stress" completed (exit code 0)`}}},
		{"a notification with no summary falls back to what it has",
			"<task-notification>\n<event>Stopped watching.</event>\n</task-notification>",
			[]*Part{{Kind: KindNote, Text: "<event>Stopped watching.</event>"}}},
		{"what a slash command printed",
			"<local-command-stdout>line one\nline two\n</local-command-stdout>",
			[]*Part{{Kind: KindOutput, Text: "line one\nline two"}}},
		{"a line typed in bash mode, and what it printed",
			"<bash-input>review</bash-input>\n<bash-stdout>a4% </bash-stdout><bash-stderr></bash-stderr>",
			[]*Part{{Kind: KindCommand, Text: "review"}, {Kind: KindOutput, Text: "a4%"}}},
		{"prose on either side is kept",
			"before\n<local-command-caveat>a caveat</local-command-caveat>\nafter",
			[]*Part{
				{Kind: KindText, Text: "before"},
				{Kind: KindNote, Text: "a caveat"},
				{Kind: KindText, Text: "after"},
			}},
		{"an unfinished tag is only prose",
			"<bash-input>review",
			[]*Part{{Kind: KindText, Text: "<bash-input>review"}}},
	}
	for _, tt := range tests {
		got := claudeText(tt.text)
		if len(got) != len(tt.want) {
			t.Errorf("%s: got %d parts, want %d: %+v", tt.name, len(got), len(tt.want), got)
			continue
		}
		for i, p := range got {
			if p.Kind != tt.want[i].Kind || p.Text != tt.want[i].Text {
				t.Errorf("%s: part %d = %v %q, want %v %q",
					tt.name, i, p.Kind, p.Text, tt.want[i].Kind, tt.want[i].Text)
			}
		}
	}
}

func TestClaudeText(t *testing.T) {
	var tests = []struct {
		text string
		kind Kind
		want string
	}{
		{"plain prose", KindText, "plain prose"},
		{"<system-reminder>gone</system-reminder>  kept", KindText, "kept"},
		{"[Request interrupted by user]", KindNote, "[Request interrupted by user]"},
		{"see [docs](x) here", KindText, "see [docs](x) here"},
		{"<command-name>/fix</command-name><command-args>all</command-args>", KindCommand, "/fix all"},
	}
	for _, tt := range tests {
		parts := claudeText(tt.text)
		if len(parts) != 1 || parts[0].Kind != tt.kind || parts[0].Text != tt.want {
			t.Errorf("claudeText(%q) = %+v, want %v %q", tt.text, parts, tt.kind, tt.want)
		}
	}
	if parts := claudeText("  <system-reminder>all gone</system-reminder> "); parts != nil {
		t.Errorf("claudeText of only a reminder = %+v, want nil", parts)
	}
}

func TestRenderHTML(t *testing.T) {
	msgs, err := claudeMessages(writeFile(t, "c.jsonl", transcript))
	if err != nil {
		t.Fatal(err)
	}
	c := &Conv{ID: "abc", Title: "Listing files", Dir: "/tmp/x"}
	html := string(renderHTML(c, msgs))
	for _, want := range []string{
		"<title>Listing files</title>",
		"<code>abc</code>",
		"<p>hello there</p>", // user prose, rendered as Markdown
		"<summary>Thinking</summary>",
		`<span class="name">Bash</span>`,
		"<pre>a\nb\n</pre>", // the tool result
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered HTML is missing %q", want)
		}
	}
}

func TestRenderHTMLEscapes(t *testing.T) {
	msgs := []*Msg{{Role: "user", Parts: []*Part{
		{Kind: KindNote, Text: `<script>alert(1)</script>`},
		{Kind: KindTool, Tool: "Bash", Args: []Arg{{"command", `<script>alert(2)</script>`}}},
	}}}
	html := string(renderHTML(&Conv{ID: "<script>alert(3)</script>"}, msgs))
	if strings.Contains(html, "<script>") {
		t.Errorf("rendered HTML contains an unescaped script tag:\n%s", html)
	}
}

func TestImageSrc(t *testing.T) {
	img := &Image{MediaType: "image/png", Data: "aGk="}
	if want := "data:image/png;base64,aGk="; string(img.Src()) != want {
		t.Errorf("Src = %q, want %q", img.Src(), want)
	}
	// Anything that is not plainly an image and its base64 is refused,
	// rather than written into the page as a URL.
	for _, bad := range []*Image{
		{MediaType: "text/html", Data: "aGk="},
		{MediaType: `image/png" onerror="alert(1)`, Data: "aGk="},
		{MediaType: "image/png", Data: `","x":"`},
	} {
		if got := bad.Src(); got != "" {
			t.Errorf("Src of %+v = %q, want \"\"", bad, got)
		}
	}
}

func TestOneLine(t *testing.T) {
	var tests = []struct{ in, want string }{
		{"short", "short"},
		{"first\nsecond", "first ..."},
		{strings.Repeat("x", briefLen+10), strings.Repeat("x", briefLen) + "..."},
	}
	for _, tt := range tests {
		if got := oneLine(tt.in); got != tt.want {
			t.Errorf("oneLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDropTag(t *testing.T) {
	var tests = []struct{ in, want string }{
		{"a<x>b</x>c", "ac"},
		{"a<x>b</x>c<x>d</x>e", "ace"},
		{"a<x>b", "a"},
		{"plain", "plain"},
	}
	for _, tt := range tests {
		if got := dropTag(tt.in, "x"); got != tt.want {
			t.Errorf("dropTag(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// sentTranscript is a transcript in which the model hands the user three
// files: an image, an image the harness did not name a type for, and a file
// that is not an image at all. The paths are filled in by the test, since
// the transcript names the files rather than holding them.
const sentTranscript = `
{"type":"assistant","uuid":"a1","timestamp":"2026-01-02T03:04:05Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"SendUserFile","input":{"files":["%[1]s"]}}]}}
{"type":"user","uuid":"u1","timestamp":"2026-01-02T03:04:06Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"3 files delivered to user."}]},"toolUseResult":{"caption":"Look at this.","attachments":[{"path":"%[1]s","isImage":true,"media_type":"image/png"},{"path":"%[2]s","isImage":true},{"path":"%[3]s","isImage":false,"media_type":"text/plain"}]}}
{"type":"assistant","uuid":"a2","timestamp":"2026-01-02T03:04:07Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"SendUserFile","input":{"files":["/does/not/exist.png"]}}]}}
{"type":"user","uuid":"u2","timestamp":"2026-01-02T03:04:08Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"1 file delivered to user."}]},"toolUseResult":{"caption":"Gone.","attachments":[{"path":"/does/not/exist.png","isImage":true,"media_type":"image/png"}]}}
`

// pngData is a 1x1 PNG, for a test that needs a real image on disk.
var pngData = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\nIDATx\x9cc\x00\x01\x00\x00\x05\x00\x01\r\n-\xb4\x00\x00\x00\x00IEND\xaeB`\x82"

func TestClaudeSent(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "shown.png")
	sniff := filepath.Join(dir, "untyped")
	text := filepath.Join(dir, "notes.txt")
	for _, f := range []struct{ name, data string }{{png, pngData}, {sniff, pngData}, {text, "hello"}} {
		if err := os.WriteFile(f.name, []byte(f.data), 0666); err != nil {
			t.Fatal(err)
		}
	}
	body := fmt.Sprintf(sentTranscript, png, sniff, text)
	msgs, err := claudeMessages(writeFile(t, "c.jsonl", body))
	if err != nil {
		t.Fatal(err)
	}
	// Both turns are the model's, so they merge, and the results add no
	// messages of their own: the two calls, with the files that arrived
	// shown just after the call that delivered them.
	if len(msgs) != 1 {
		t.Fatalf("claudeMessages returned %d messages, want 1", len(msgs))
	}
	parts := msgs[0].Parts
	if len(parts) != 3 {
		t.Fatalf("message has %d parts, want 3: %+v", len(parts), parts)
	}
	// The second delivery names a file that is not there, so it shows nothing.
	if parts[0].Kind != KindTool || parts[2].Kind != KindTool {
		t.Fatalf("parts = %+v, want the file between the two calls", parts)
	}
	p := parts[1]
	if p.Kind != KindFile || p.Text != "Look at this." {
		t.Fatalf("part = %+v, want the caption of the first delivery", p)
	}
	// The image, and the one whose type had to be sniffed. Not the text file.
	if len(p.Images) != 2 {
		t.Fatalf("part has %d images, want 2", len(p.Images))
	}
	for i, img := range p.Images {
		if img.MediaType != "image/png" {
			t.Errorf("image %d media type = %q, want image/png", i, img.MediaType)
		}
		if got := string(img.Src()); !strings.HasPrefix(got, "data:image/png;base64,iVBOR") {
			t.Errorf("image %d src = %q, want an inline PNG", i, got)
		}
	}
	// A file shown to the user is something the model said, not a detail of
	// the call that carried it, so it survives with the thinking and the
	// tool calls stripped away.
	kept := onlySaid(msgs)
	if len(kept) != 1 || len(kept[0].Parts) != 1 || kept[0].Parts[0] != p {
		t.Errorf("onlySaid dropped the delivered file: %+v", kept)
	}
	if html := string(renderHTML(&Conv{ID: "x"}, kept)); !strings.Contains(html, "<figcaption>Look at this.</figcaption>") {
		t.Errorf("rendered HTML is missing the caption")
	}
}

func TestReadImage(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.png")
	if err := os.WriteFile(big, make([]byte, maxImage+1), 0666); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{big, dir, filepath.Join(dir, "missing.png")} {
		if img := readImage(file, "image/png"); img != nil {
			t.Errorf("readImage(%q) = %+v, want nil", file, img)
		}
	}
}
