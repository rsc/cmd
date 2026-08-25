// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"iter"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Claude Code records each conversation as a JSON Lines file
// $HOME/.claude/projects/<escaped directory>/<conversation ID>.jsonl,
// holding one record per line. The records are written as the
// conversation proceeds, so the last title record in the file
// holds the current title.

// A claudeRec is one record in a Claude Code transcript.
// Only the fields agentweb uses are listed here.
type claudeRec struct {
	Type        string     `json:"type"`
	Timestamp   time.Time  `json:"timestamp"`
	Cwd         string     `json:"cwd"`
	IsMeta      bool       `json:"isMeta"`
	IsSidechain bool       `json:"isSidechain"`
	Message     *claudeMsg `json:"message"`
	CustomTitle string     `json:"customTitle"`
	AITitle     string     `json:"aiTitle"`
}

// A claudeMsg is the model message in a "user" or "assistant" record.
type claudeMsg struct {
	Role    string     `json:"role"`
	Content claudeBody `json:"content"`
}

// A claudeBody is a message body. In the JSON it is either a bare
// string, which is shorthand for a single text block, or a list of blocks.
type claudeBody []claudeBlock

func (b *claudeBody) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*b = claudeBody{{Type: "text", Text: s}}
		return nil
	}
	var list []claudeBlock
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	*b = list
	return nil
}

// A claudeBlock is one content block in a message body.
type claudeBlock struct {
	Type      string                     `json:"type"`
	Text      string                     `json:"text"`
	Thinking  string                     `json:"thinking"`
	ID        string                     `json:"id"`
	Name      string                     `json:"name"`
	Input     map[string]json.RawMessage `json:"input"`
	ToolUseID string                     `json:"tool_use_id"`
	Content   claudeBody                 `json:"content"`
	IsError   bool                       `json:"is_error"`
	Source    *claudeSource              `json:"source"`
}

// A claudeSource is the image carried by an "image" block.
type claudeSource struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// claudeConvs returns the conversations recorded under dir,
// which should be $HOME/.claude.
func claudeConvs(dir string) []*Conv {
	files, err := filepath.Glob(filepath.Join(dir, "projects", "*", "*.jsonl"))
	if err != nil {
		log.Print(err)
		return nil
	}
	var convs []*Conv
	for _, file := range files {
		c, err := claudeConv(file)
		if err != nil {
			log.Print(err)
			continue
		}
		convs = append(convs, c)
	}
	return convs
}

// claudeScan is how much of a transcript claudeConv reads from each end
// looking for the conversation's title, directory, and last activity time.
const claudeScan = 1 << 20

// claudeConv describes the conversation recorded in file,
// without reading the whole transcript.
func claudeConv(file string) (*Conv, error) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	c := &Conv{
		ID:   strings.TrimSuffix(filepath.Base(file), ".jsonl"),
		Time: info.ModTime(),
		File: file,
	}

	// The title and the final timestamp are near the end of the transcript.
	tail, err := readTail(file, claudeScan)
	if err != nil {
		return nil, err
	}
	var aiTitle string
	for rec := range claudeRecs(bytes.NewReader(tail)) {
		switch {
		case rec.CustomTitle != "":
			c.Title = rec.CustomTitle
		case rec.AITitle != "":
			aiTitle = rec.AITitle
		}
		if rec.Cwd != "" {
			c.Dir = rec.Cwd
		}
		if !rec.Timestamp.IsZero() {
			c.Time = rec.Timestamp
		}
	}
	if c.Title == "" {
		c.Title = aiTitle
	}
	if c.Title != "" && c.Dir != "" {
		return c, nil
	}

	// A conversation too short to have been given a title,
	// or one long enough that a single record filled the tail.
	// Fall back to what the start of the transcript says.
	head, err := readHead(file, claudeScan)
	if err != nil {
		return nil, err
	}
	for rec := range claudeRecs(bytes.NewReader(head)) {
		if c.Dir == "" {
			c.Dir = rec.Cwd
		}
		if c.Title != "" || rec.Message == nil || rec.Message.Role != "user" || rec.IsMeta {
			continue
		}
		// The first thing the user actually said. Notifications and command
		// output arrive in the user's messages too, and name nothing.
		for _, b := range rec.Message.Content {
			if b.Type != "text" {
				continue
			}
			for _, p := range claudeText(b.Text) {
				if p.Kind == KindText || p.Kind == KindCommand {
					c.Title = oneLine(p.Text)
					break
				}
			}
			if c.Title != "" {
				break
			}
		}
	}
	if c.Title == "" {
		c.Title = "(untitled)"
	}
	return c, nil
}

// claudeMessages reads the Claude Code transcript in file.
func claudeMessages(file string) ([]*Msg, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var msgs []*Msg
	tools := make(map[string]*Part) // tool call ID -> call awaiting its result
	for rec := range claudeRecs(f) {
		if rec.Message == nil || rec.IsMeta || rec.IsSidechain {
			continue
		}
		var parts []*Part
		for _, b := range rec.Message.Content {
			switch b.Type {
			case "text":
				parts = append(parts, claudeText(b.Text)...)
			case "thinking":
				if s := strings.TrimSpace(b.Thinking); s != "" {
					parts = append(parts, &Part{Kind: KindThinking, Text: s})
				}
			case "tool_use":
				p := &Part{Kind: KindTool, Tool: b.Name, Args: claudeArgs(b.Input)}
				tools[b.ID] = p
				parts = append(parts, p)
			case "tool_result":
				// The result belongs to the call, which is in an earlier message.
				if p := tools[b.ToolUseID]; p != nil {
					p.Result, p.Images = claudeResult(b.Content)
					p.IsError = b.IsError
					delete(tools, b.ToolUseID)
				}
			case "image":
				if b.Source != nil {
					img := &Image{MediaType: b.Source.MediaType, Data: b.Source.Data}
					parts = append(parts, &Part{Kind: KindText, Images: []*Image{img}})
				}
			}
		}
		if len(parts) == 0 {
			continue
		}
		// A single turn is several records: the model's text and tool calls,
		// then the tool results, then more text and calls. The results have
		// been folded into the calls above, so runs of records with the same
		// role are all one message. The harness's own reports are filed under
		// the user's role but are not part of the user's turn, so they neither
		// join a message nor take one in.
		n := len(msgs)
		if n > 0 && msgs[n-1].Role == rec.Message.Role && msgs[n-1].Aside() == aside(parts) {
			msgs[n-1].Parts = append(msgs[n-1].Parts, parts...)
			continue
		}
		msgs = append(msgs, &Msg{Role: rec.Message.Role, Time: rec.Timestamp, Parts: parts})
	}
	return msgs, nil
}

// claudeText converts the text of a message into parts, pulling out slash
// commands and dropping the system reminders the harness injects into
// user messages.
func claudeText(text string) []*Part {
	if name, args, ok := claudeCommand(text); ok {
		return []*Part{{Kind: KindCommand, Text: strings.TrimSpace(name + " " + args)}}
	}
	text = dropTag(text, "system-reminder")

	// The harness writes text of its own into the user's messages, wrapped in
	// tags saying what it is. Pull each piece out and label it, so that none
	// of it reads as something the user said. Left in place, it would not even
	// read as itself: the Markdown passes the tags through as HTML, and the
	// browser drops them, running the text they separated together.
	var parts []*Part
	for {
		tag, inner, before, rest, ok := splitHarness(text)
		if !ok {
			return append(parts, claudeProse(text)...)
		}
		parts = append(parts, claudeProse(before)...)
		if p := harnessPart(tag, inner); p != nil {
			parts = append(parts, p)
		}
		text = rest
	}
}

// claudeProse converts what is left after the harness's own text is taken out.
func claudeProse(text string) []*Part {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	// Notes like "[Request interrupted by user]" come from the harness,
	// not from the user or the model.
	if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") &&
		!strings.ContainsAny(text[1:len(text)-1], "[]\n") {
		return []*Part{{Kind: KindNote, Text: text}}
	}
	return []*Part{{Kind: KindText, Text: text}}
}

// harnessKind gives the kind of part to make from the text the harness wraps
// in each of the tags it uses. A tag not named here is left where it is, as
// part of the prose around it.
var harnessKind = map[string]Kind{
	"task-notification":    KindNote,   // a background task finished
	"local-command-caveat": KindNote,   // an aside about a slash command
	"local-command-stdout": KindOutput, // what a slash command printed
	"bash-input":           KindCommand,
	"bash-stdout":          KindOutput,
	"bash-stderr":          KindOutput,
}

// harnessPart returns the part to show for the text the harness wrapped in
// tag, or nil if there is nothing in it worth showing.
func harnessPart(tag, inner string) *Part {
	if tag == "task-notification" {
		// Bookkeeping for the harness — a task ID, the tool call it came
		// from, the file holding the output — surrounds a single line
		// written for a person to read. Show only that line.
		if s, ok := tagText(inner, "summary"); ok {
			inner = s
		}
	}
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return nil
	}
	return &Part{Kind: harnessKind[tag], Text: inner}
}

// splitHarness finds the first element in text whose tag is one the harness
// uses, returning the tag, the text inside it, and the text on either side.
func splitHarness(text string) (tag, inner, before, rest string, ok bool) {
	start := -1
	for name := range harnessKind {
		open, close := "<"+name+">", "</"+name+">"
		i := strings.Index(text, open)
		if i < 0 || start >= 0 && i > start {
			continue
		}
		j := strings.Index(text[i+len(open):], close)
		if j < 0 {
			continue // an opening tag with no end is not an element
		}
		start, tag = i, name
		before = text[:i]
		inner = text[i+len(open) : i+len(open)+j]
		rest = text[i+len(open)+j+len(close):]
	}
	return tag, inner, before, rest, start >= 0
}

// claudeCommand reports whether text is a slash command invocation,
// which the harness records as <command-name> and <command-args> elements,
// and if so returns the command's name and arguments.
func claudeCommand(text string) (name, args string, ok bool) {
	name, ok = tagText(text, "command-name")
	if !ok {
		return "", "", false
	}
	args, _ = tagText(text, "command-args")
	return strings.TrimSpace(name), strings.TrimSpace(args), true
}

// claudeArgs converts a tool call's JSON input into arguments to display,
// ordering the most descriptive ones first.
func claudeArgs(input map[string]json.RawMessage) []Arg {
	keys := slices.Sorted(maps.Keys(input))
	slices.SortStableFunc(keys, func(x, y string) int {
		return briefIndex(x) - briefIndex(y)
	})
	var args []Arg
	for _, key := range keys {
		args = append(args, Arg{key, jsonText(input[key])})
	}
	return args
}

// briefIndex returns key's position in [briefKeys],
// or a larger number if it is not there at all.
func briefIndex(key string) int {
	if i := slices.Index(briefKeys, key); i >= 0 {
		return i
	}
	return len(briefKeys)
}

// jsonText renders a JSON value as text: a string as itself,
// anything else as indented JSON.
func jsonText(data []byte) string {
	var s string
	if json.Unmarshal(data, &s) == nil {
		return s
	}
	var buf bytes.Buffer
	if json.Indent(&buf, data, "", "\t") != nil {
		return string(data)
	}
	return buf.String()
}

// claudeResult splits a tool result into the text it printed
// and the images it returned.
func claudeResult(body claudeBody) (string, []*Image) {
	var text strings.Builder
	var images []*Image
	for _, b := range body {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "image":
			if b.Source != nil {
				images = append(images, &Image{MediaType: b.Source.MediaType, Data: b.Source.Data})
			}
		}
	}
	return text.String(), images
}

// claudeRecs returns an iterator over the records in a Claude Code transcript,
// skipping any line that does not decode. Transcripts are appended to as the
// conversation runs, so the last line of one may be incomplete, and callers
// pass in fragments that start mid-line.
func claudeRecs(r io.Reader) iter.Seq[*claudeRec] {
	return func(yield func(*claudeRec) bool) {
		b := bufio.NewReader(r)
		for {
			line, err := b.ReadBytes('\n')
			if len(line) > 0 {
				var rec claudeRec
				if json.Unmarshal(line, &rec) == nil {
					if !yield(&rec) {
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}
}

// readHead returns up to the first n bytes of file.
func readHead(file string, n int64) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, n))
}

// readTail returns up to the last n bytes of file,
// trimmed at the start to begin on a line boundary.
func readTail(file string, n int64) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if off := info.Size() - n; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, err
		}
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
		return data, nil
	}
	return io.ReadAll(f)
}

// tagText returns the text inside the first <tag>...</tag> element in text.
func tagText(text, tag string) (string, bool) {
	open, close := "<"+tag+">", "</"+tag+">"
	i := strings.Index(text, open)
	if i < 0 {
		return "", false
	}
	rest := text[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// dropTag removes every <tag>...</tag> element from text.
func dropTag(text, tag string) string {
	open, close := "<"+tag+">", "</"+tag+">"
	for {
		i := strings.Index(text, open)
		if i < 0 {
			return text
		}
		j := strings.Index(text[i:], close)
		if j < 0 {
			return text[:i]
		}
		text = text[:i] + text[i+j+len(close):]
	}
}
