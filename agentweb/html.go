// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"html/template"
	"log"
	"regexp"
	"strings"
	"time"

	"rsc.io/markdown"
)

// maxResult is the longest tool result the HTML shows.
// Some tools print megabytes; the rest is elided.
const maxResult = 40 << 10

// KindName returns the part's kind as a string, for the template.
func (p *Part) KindName() string {
	switch p.Kind {
	case KindText:
		return "text"
	case KindThinking:
		return "thinking"
	case KindTool:
		return "tool"
	case KindCommand:
		return "command"
	case KindOutput:
		return "output"
	case KindNote:
		return "note"
	}
	return "unknown"
}

// A Group is a run of a message's parts shown together: either a single part
// on its own, or a run of tool calls gathered behind one expander.
type Group struct {
	Part  *Part   // the part to show, when Tools is empty
	Tools []*Part // the run of tool calls, otherwise
}

// Groups returns the message's parts arranged for display, gathering each run
// of tool calls so that a long stretch of them takes up a single line.
// Thinking between two calls ends the run, keeping it one click away rather
// than two. A run of one is gathered like any other, so that every call in the
// conversation sits behind the same two expanders.
func (m *Msg) Groups() []*Group {
	var groups []*Group
	for i := 0; i < len(m.Parts); {
		j := i
		for j < len(m.Parts) && m.Parts[j].Kind == KindTool {
			j++
		}
		if j > i {
			groups = append(groups, &Group{Tools: m.Parts[i:j]})
			i = j
			continue
		}
		groups = append(groups, &Group{Part: m.Parts[i]})
		i++
	}
	return groups
}

// Label describes a run of tool calls, counting the calls to each tool in the
// order the tools are first used: "Ran tools (7 Bash, 1 WebFetch)".
func (g *Group) Label() string {
	var names []string
	n := make(map[string]int)
	for _, p := range g.Tools {
		if n[p.Tool] == 0 {
			names = append(names, p.Tool)
		}
		n[p.Tool]++
	}
	var b strings.Builder
	b.WriteString("Ran tools (")
	for i, name := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d %s", n[name], name)
	}
	b.WriteString(")")
	return b.String()
}

// ResultText returns the tool result to show, shortened if it is very long.
func (p *Part) ResultText() string {
	if len(p.Result) <= maxResult {
		return p.Result
	}
	return fmt.Sprintf("%s\n\n... %d more bytes elided ...\n", p.Result[:maxResult], len(p.Result)-maxResult)
}

// mediaType matches the image media types we are willing to
// write into a data: URL.
var mediaType = regexp.MustCompile(`^image/[a-zA-Z0-9.+-]+$`)

// base64Text matches base64-encoded data.
var base64Text = regexp.MustCompile(`^[A-Za-z0-9+/\r\n]*={0,2}$`)

// Src returns the image as a data: URL, or the empty string
// if the transcript's image does not look like an image at all.
func (i *Image) Src() template.URL {
	if !mediaType.MatchString(i.MediaType) || !base64Text.MatchString(i.Data) {
		return ""
	}
	return template.URL("data:" + i.MediaType + ";base64," + i.Data)
}

// renderHTML formats a conversation as a standalone HTML page.
func renderHTML(c *Conv, msgs []*Msg) []byte {
	var buf bytes.Buffer
	data := struct {
		Conv *Conv
		Msgs []*Msg
	}{c, msgs}
	if err := page.Execute(&buf, data); err != nil {
		log.Fatal(err)
	}
	return buf.Bytes()
}

// mdHTML renders Markdown text as HTML.
func mdHTML(text string) template.HTML {
	p := &markdown.Parser{
		AutoLinkText:  true,
		Strikethrough: true,
		Table:         true,
		TaskList:      true,
	}
	return template.HTML(markdown.ToHTML(p.Parse(text)))
}

// stamp formats a time for display, or returns "" for the zero time.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("Mon 2 Jan 2006 15:04:05")
}

// roleName returns the name to show for a message's role.
func roleName(role string) string {
	switch role {
	case "user":
		return "You"
	case "assistant":
		return "Assistant"
	}
	return role
}

var page = template.Must(template.New("page").Funcs(template.FuncMap{
	"md":    mdHTML,
	"stamp": stamp,
	"role":  roleName,
}).Parse(pageHTML))

const pageHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Conv.Title}}</title>
<style>
/*
 * Page styles taken from research.swtch.com, so that this conversation reads
 * the same here and there. The two font families are named but not loaded:
 * on research.swtch.com the site's own @font-face rules supply them, and
 * here they come from the fonts installed on the machine, falling back to
 * the generic serif and monospace when there are none.
 */
body {
	padding: 0;
	margin: 0;
	font-size: 100%;
	font-family: 'Minion 3', serif;
}
.main {
	position: relative;
	margin: 0 auto;
	padding: 0;
	width: 900px;
}
@media only screen and (min-width: 768px) and (max-width: 959px) { .main { width: 708px; } }
@media only screen and (min-width: 640px) and (max-width: 767px) { .main { width: 580px; } }
@media only screen and (min-width: 480px) and (max-width: 639px) { .main { width: 420px; } }
@media only screen and (max-width: 479px) { .main { width: 300px; } }
@media only screen and (max-width: 479px) { .article { font-size: 120%; } }
.article h1 { text-align: center; font-size: 200%; margin-bottom: 0.25em; }
.article h2 { font-size: 125%; padding-top: 0.25em; }
.article h3 { font-size: 100%; }
.article p, .article ol { line-height: 144%; }
.subtitle { font-size: 83%; }
pre, code { font-family: 'Source Code Pro', monospace; font-size: 90%; }
code { word-spacing: -0.25em; }
pre code { word-spacing: normal; font-size: 100%; }
table {
	border-top: 2px solid black;
	border-bottom: 1px solid black;
	border-collapse: collapse;
	margin: 0 auto;
}
thead tr { border-bottom: 1px solid black; }
th, td { padding-left: 1ex; padding-right: 1ex; text-align: left; }
sup, sub { vertical-align: baseline; position: relative; font-size: 83%; }
sup { bottom: 1ex; }
sub { top: 0.8ex; }
img { max-width: 100%; }

/*
 * The conversation itself. None of this survives a copy into an article,
 * where the messages become plain headings and paragraphs, so it holds
 * only what makes the page readable on its own.
 */
.subtitle { text-align: center; color: #666; margin-top: 0; margin-bottom: 2.5em; }
.msg { margin: 1.5em 0; }
.msg.user { background: #ffffe9; border: 1px solid #ccc; padding: 0.5em 1em; }
/* The harness reporting, in neither side's voice and so in neither's box. */
.msg.aside { margin: 0.8em 0; }
/*
 * Who is speaking, and when. A heading rather than a styled div, so that a
 * copy of this page into an article still shows the turns, in the article's
 * own h3. The space between the two in the markup matters there, where the
 * flex box that separates them here does not apply.
 */
.article h3.who {
	display: flex;
	justify-content: space-between;
	font-size: 83%;
	font-style: italic;
	font-weight: normal;
	color: #666;
	margin: 0 0 0.4em;
}
/*
 * A run of prose is one of these for each time the model stopped to call a
 * tool, so the space between them belongs to the block, not to the single
 * paragraph inside it, which is both the first child and the last.
 */
.text { margin: 0.8em 0; }
.text > :first-child { margin-top: 0; }
.text > :last-child { margin-bottom: 0; }
.text pre {
	background: #f8f8f8;
	border: 1px solid #ddd;
	margin-left: 2em;
	margin-right: 2em;
	padding: 0.5em;
	overflow-x: auto;
}
.text blockquote { border-left: 2px solid #ccc; margin-left: 0; padding-left: 1em; color: #666; }
details { margin: 0.5em 0; }
details > summary {
	cursor: pointer;
	color: #666;
	white-space: nowrap;
	overflow: hidden;
	text-overflow: ellipsis;
}
.thinking > summary { font-style: italic; }
.thinking .text { border-left: 2px solid #ddd; margin-left: 1em; padding-left: 1em; color: #444; }
.tool .name { color: black; font-weight: 600; }
.tool.error .name { color: #900; }
.tool .name, .tool .brief { font-family: 'Source Code Pro', monospace; font-size: 90%; }
.arg { margin: 0.4em 0 0.4em 2em; }
.arg .key { font-style: italic; font-size: 83%; color: #666; }
.arg pre {
	background: #f8f8f8;
	border: 1px solid #ddd;
	margin: 0.15em 0;
	padding: 0.5em;
	max-height: 32em;
	overflow: auto;
	white-space: pre-wrap;
	overflow-wrap: anywhere;
}
.tools > summary { font-style: italic; }
.tools > details { margin-left: 1.5em; }
.command { font-family: 'Source Code Pro', monospace; font-size: 90%; }
.output {
	background: #f8f8f8;
	border: 1px solid #ddd;
	margin: 0.4em 0;
	padding: 0.5em;
	max-height: 32em;
	overflow: auto;
}
.note { font-style: italic; font-size: 83%; color: #666; }
</style>
</head>
<body>

<div class="main">
<div class="article">

<h1>{{.Conv.Title}}</h1>
<p class="subtitle">{{with .Conv.Dir}}<code>{{.}}</code> &middot; {{end}}{{stamp .Conv.Time}} &middot; {{len .Msgs}} messages<br><code>{{.Conv.ID}}</code></p>

{{range .Msgs}}
<div class="msg {{.Class}}">
{{- if not .Aside}}
<h3 class="who">{{role .Role}}{{if not .Time.IsZero}} <span class="at">{{stamp .Time}}</span>{{end}}</h3>
{{- end}}
{{- range .Groups}}
{{- if .Tools}}
<details class="tools"><summary>{{.Label}}</summary>
{{- range .Tools}}{{template "part" .}}
{{- end}}
</details>
{{- else}}{{template "part" .Part}}
{{- end}}
{{- end}}
</div>
{{end}}

</div>
</div>

</body>
</html>
{{define "part"}}
{{- if eq .KindName "text"}}
<div class="text">{{md .Text}}</div>{{template "images" .}}
{{- else if eq .KindName "thinking"}}
<details class="thinking"><summary>Thinking</summary><div class="text">{{md .Text}}</div></details>
{{- else if eq .KindName "command"}}
<div class="command">{{.Text}}</div>
{{- else if eq .KindName "output"}}
<pre class="output">{{.Text}}</pre>
{{- else if eq .KindName "note"}}
<div class="note">{{.Text}}</div>
{{- else if eq .KindName "tool"}}
<details class="tool{{if .IsError}} error{{end}}"><summary><span class="name">{{.Tool}}</span> <span class="brief">{{.Brief}}</span></summary>
{{- range .Args}}
<div class="arg"><div class="key">{{.Key}}</div><pre>{{.Value}}</pre></div>
{{- end}}
{{- with .ResultText}}
<div class="arg"><div class="key">Result</div><pre>{{.}}</pre></div>
{{- end}}
{{- template "images" .}}
</details>
{{- end}}
{{- end}}
{{define "images"}}{{range .Images}}{{with .Src}}
<img src="{{.}}">{{end}}{{end}}{{end}}
`
