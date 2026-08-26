// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "time"

// A Conv describes a conversation stored on disk.
// The messages are only read on demand, by [Conv.Messages].
type Conv struct {
	ID    string    // conversation ID, also the transcript file name
	Title string    // conversation title
	Dir   string    // directory the conversation was about
	Time  time.Time // time of the last activity in the conversation
	File  string    // file holding the transcript
}

// Messages reads and returns the messages in the conversation.
func (c *Conv) Messages() ([]*Msg, error) {
	return claudeMessages(c.File)
}

// A Msg is one message in a conversation.
type Msg struct {
	Role  string // "user" or "assistant"
	Time  time.Time
	Parts []*Part
}

// Aside reports whether the message is only the harness reporting something —
// a background task finishing, what a command printed — rather than either
// side of the conversation speaking. The transcript files these under the
// user's role, but the user did not say them, and they are shown plainly
// instead of in the user's voice.
func (m *Msg) Aside() bool {
	return aside(m.Parts)
}

// aside reports whether parts hold nothing that either side said.
func aside(parts []*Part) bool {
	for _, p := range parts {
		switch p.Kind {
		case KindText, KindCommand, KindThinking, KindTool, KindFile:
			return false
		}
	}
	return true
}

// Class returns the class the page gives the message, which is the speaker
// for something either side said and "aside" for the harness's own reports.
func (m *Msg) Class() string {
	if m.Aside() {
		return "aside"
	}
	return m.Role
}

// onlySaid returns the messages with the model's thinking and its tool calls
// removed, leaving only what the two sides said to each other. A turn that
// was nothing but tool calls drops out entirely, rather than showing as an
// empty message.
func onlySaid(msgs []*Msg) []*Msg {
	var keep []*Msg
	for _, m := range msgs {
		var parts []*Part
		for _, p := range m.Parts {
			if p.Kind != KindThinking && p.Kind != KindTool {
				parts = append(parts, p)
			}
		}
		if len(parts) > 0 {
			keep = append(keep, &Msg{Role: m.Role, Time: m.Time, Parts: parts})
		}
	}
	return keep
}

// A Part is one piece of a message.
// Which fields are set depends on the [Kind].
type Part struct {
	Kind    Kind
	Text    string   // KindText, KindThinking, KindCommand, KindNote, KindFile (the caption)
	Images  []*Image // KindText, KindTool, KindFile
	Tool    string   // KindTool: tool name
	Args    []Arg    // KindTool: tool arguments
	Result  string   // KindTool: what the tool printed
	IsError bool     // KindTool: the tool reported an error
}

// A Kind is the kind of content in a [Part].
type Kind int

const (
	KindText     Kind = iota // Markdown prose
	KindThinking             // the model's private reasoning
	KindTool                 // a tool call and its result
	KindCommand              // a slash command the user typed
	KindOutput               // what a command printed, shown as it came
	KindNote                 // an out-of-band note, like an interruption
	KindFile                 // a file the model handed to the user
)

// An Arg is a single named argument to a tool call.
type Arg struct {
	Key   string
	Value string
}

// An Image is an image included in a message or a tool result.
type Image struct {
	MediaType string // like "image/png"
	Data      string // the image, base64 encoded
}

// briefKeys lists the tool arguments that best summarize a tool call,
// most descriptive first. [Part.Brief] shows the first one a call has.
var briefKeys = []string{
	"command", "file_path", "path", "pattern", "notebook_path",
	"url", "query", "description", "prompt", "skill", "todos",
}

// Brief returns a one-line summary of a tool call's arguments,
// for the collapsed form of the call.
func (p *Part) Brief() string {
	for _, key := range briefKeys {
		for _, a := range p.Args {
			if a.Key == key {
				return oneLine(a.Value)
			}
		}
	}
	if len(p.Args) == 1 {
		return oneLine(p.Args[0].Value)
	}
	return ""
}

// briefLen is the longest summary [Part.Brief] returns.
const briefLen = 120

// oneLine returns the first line of s, shortened to at most [briefLen] characters.
func oneLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i] + " ..."
		}
		if i >= briefLen {
			return s[:i] + "..."
		}
	}
	return s
}
