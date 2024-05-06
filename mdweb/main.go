// Mdweb

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"rsc.io/markdown"
)

var (
	addr = flag.String("a", "localhost:8780", "serve HTTP requests on `addr`")
	root = flag.String("r", ".", "set `root` directory for serving content")

	dir http.FileSystem
	fs  http.Handler
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
	if checkLastModified(w, req, info.ModTime()) {
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
		HeadingIDs: true,
		Strikethrough: true,
		TaskListItems: true,
		AutoLinkText: true,
		Table: true,
		Emoji: true,
		SmartDot: true,
		SmartDash: true,
		SmartQuote: true,
	}
	doc := p.Parse(string(data))
	html := markdown.ToHTML(doc)
	w.Write([]byte(html))
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
