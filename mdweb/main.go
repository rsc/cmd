// Mdweb serves rendered Markdown from the current directory on localhost:8780.
//
// Usage:
//
//	mdweb [-a addr] [-r root]
//
// The -a flag sets a different service address (default localhost:8780).
//
// The -r flag sets a different root directory to serve (default current directory).
//
// Files named *-talk.md are served as slide presentations, using an
// embedded style sheet and script: each # heading starts a new slide,
// and anything after a --- rule on a slide is speaker notes.
// Press ? in the browser for the list of presentation keys.
//
// If the first line of the first slide's notes is a line like
//
//	time: 20m
//
// then the presenter view schedules the talk, dividing the time
// among the slides in proportion to the length of their notes.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"rsc.io/markdown"
)

// Slides mode, for files named *-talk.md: each # heading starts a new slide.
var (
	//go:embed slides.css
	slidesCSS string

	//go:embed slides.js
	slidesJS string
)

var (
	addr = flag.String("a", "localhost:8780", "serve HTTP requests on `addr`")
	root = flag.String("r", ".", "set `root` directory for serving content")

	dir http.FileSystem
	fs  http.Handler

	// start is the process start time. Slide pages embed the CSS and
	// JavaScript from the binary, so they are only as old as the binary.
	start = time.Now()
)

func usage() {
	fmt.Fprintf(os.Stderr, "usage: mdweb [-a addr] [-r root]\n")
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	log.SetPrefix("mdweb: ")
	log.SetFlags(0)

	flag.Usage = usage
	flag.Parse()
	if flag.NArg() != 0 {
		usage()
	}

	dir = http.Dir(*root)
	fs = http.FileServer(dir)
	http.HandleFunc("/", md)
	fmt.Fprintf(os.Stderr, "mdweb: serving %s on http://%s\n", *root, *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func md(w http.ResponseWriter, req *http.Request) {
	if req.Method != "GET" {
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
		return
	}

	if !strings.HasSuffix(req.URL.Path, ".md") {
		fs.ServeHTTP(w, req)
		return
	}

	f, err := dir.Open(req.URL.Path)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	modTime := info.ModTime()
	if isTalk(req.URL.Path) && modTime.Before(start) {
		modTime = start
	}
	if checkLastModified(w, req, modTime) {
		f.Close()
		return
	}

	data, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		http.Error(w, "error reading data", http.StatusInternalServerError)
		return
	}

	p := &markdown.Parser{
		HeadingID:     true,
		Strikethrough: true,
		TaskList:      true,
		AutoLinkText:  true,
		Table:         true,
		Emoji:         true,
		SmartDot:      true,
		SmartDash:     true,
		SmartQuote:    true,
	}
	doc := p.Parse(string(data))
	body := markdown.ToHTML(doc)
	if isTalk(req.URL.Path) {
		slides(w, req, doc, body)
		return
	}
	w.Write([]byte(`<!DOCTYPE html>`))
	w.Write([]byte(body))
}

// isTalk reports whether path names a slide presentation.
func isTalk(path string) bool {
	return strings.HasSuffix(path, "-talk.md")
}

// slides writes doc's rendered HTML wrapped in the slide CSS and JavaScript,
// which split the document into one slide per # heading.
func slides(w http.ResponseWriter, req *http.Request, doc *markdown.Document, body string) {
	title := docTitle(doc)
	if title == "" {
		title = strings.TrimSuffix(path.Base(req.URL.Path), ".md")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<style>
%s</style>
</head>
<body>
<div id="deck">
%s</div>
<div id="status"></div>
<div id="notes"></div>
<div id="pageno"></div>
<div id="msg"></div>
<div id="help">%s</div>
<script>
%s</script>
</body>
</html>
`, html.EscapeString(title), slidesCSS, body, helpHTML, slidesJS)
}

const helpHTML = `<table>
<tr><td><kbd>&rarr;</kbd> <kbd>space</kbd> <kbd>n</kbd></td><td>next slide</td></tr>
<tr><td><kbd>&larr;</kbd> <kbd>p</kbd></td><td>previous slide</td></tr>
<tr><td><kbd>home</kbd> <kbd>end</kbd></td><td>first, last slide</td></tr>
<tr><td><kbd>o</kbd> <kbd>esc</kbd></td><td>slide overview</td></tr>
<tr><td><kbd>f</kbd></td><td>full screen</td></tr>
<tr><td><kbd>P</kbd></td><td>present: slides in a new window, notes here</td></tr>
<tr><td><kbd>R</kbd></td><td>reset the talk timer</td></tr>
<tr><td><kbd>?</kbd></td><td>this help</td></tr>
</table>`

// docTitle returns the plain text of the document's first heading, if any.
func docTitle(doc *markdown.Document) string {
	for _, b := range doc.Blocks {
		if h, ok := b.(*markdown.Heading); ok {
			return textOnly(markdown.ToHTML(h))
		}
	}
	return ""
}

// textOnly returns the HTML fragment h with its tags removed.
func textOnly(h string) string {
	var buf strings.Builder
	for {
		i := strings.IndexByte(h, '<')
		if i < 0 {
			break
		}
		buf.WriteString(h[:i])
		j := strings.IndexByte(h[i:], '>')
		if j < 0 {
			return html.UnescapeString(strings.TrimSpace(buf.String()))
		}
		h = h[i+j+1:]
	}
	buf.WriteString(h)
	return html.UnescapeString(strings.TrimSpace(buf.String()))
}

// copied from net/http

var unixEpochTime = time.Unix(0, 0)

// modtime is the modification time of the resource to be served, or IsZero().
// return value is whether this request is now complete.
func checkLastModified(w http.ResponseWriter, r *http.Request, modtime time.Time) bool {
	if modtime.IsZero() || modtime.Equal(unixEpochTime) {
		// If the file doesn't have a modtime (IsZero), or the modtime
		// is obviously garbage (Unix time == 0), then ignore modtimes
		// and don't process the If-Modified-Since header.
		return false
	}

	// The Date-Modified header truncates sub-second precision, so
	// use mtime < t+1s instead of mtime <= t to check for unmodified.
	if t, err := time.Parse(http.TimeFormat, r.Header.Get("If-Modified-Since")); err == nil && modtime.Before(t.Add(1*time.Second)) {
		h := w.Header()
		delete(h, "Content-Type")
		delete(h, "Content-Length")
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	w.Header().Set("Last-Modified", modtime.UTC().Format(http.TimeFormat))
	return false
}
