// Copyright 2014 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// C2gofmt translates C syntax source files into Go syntax.
//
// Usage:
//
//	c2gofmt [-v] [-w] [-r file] [file.c file.h ...]
//
// C2gofmt translates the named C source files to Go syntax.
// It only operates syntactically: it does not type-check the C code
// nor the generated Go code. As a result, the generated Go code
// will almost certainly not compile. But it can serve well as the
// starting point for a manual translation, with c2gofmt having
// done much of the most tedious work.
//
// The -v flag causes c2gofmt to print verbose output.
//
// By default, c2gofmt writes the Go translation on standard output.
// The -w flag causes c2gofmt to write a Go file for each C input file,
// named by removing the .c suffix (if any) and adding .go.
//
// The -r flag causes c2gofmt to read rewrite rules from the named file.
// In the file, blank lines or lines beginning with # are ignored.
// Other lines take the form “old -> new” and are interpreted the
// same as the patterns passed to “gofmt -r”.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

	"rsc.io/cmd/c2gofmt/internal/cc"
)

var (
	rulefile = flag.String("r", "", "load rewrite rules from `file`")
	flagW    = flag.Bool("w", false, "write files")
	verbose  = flag.Bool("v", false, "print verbose output")
)

func usage() {
	fmt.Fprintf(os.Stderr, "usage: c2gofmt [-w] [-r rulefile] [file.c ...]\n")
	os.Exit(2)
}

func main() {
	log.SetPrefix("c2gofmt: ")
	log.SetFlags(0)
	flag.Usage = usage
	flag.Parse()

	if *rulefile != "" {
		data, err := ioutil.ReadFile(*rulefile)
		if err != nil {
			log.Fatal(err)
		}
		parseRules(*rulefile, string(data))
	}

	args := flag.Args()
	if len(args) == 0 {
		data, err := ioutil.ReadAll(os.Stdin)
		if err != nil {
			log.Fatal(err)
		}
		os.Stdout.Write(do("stdin", data))
		return
	}

	for _, name := range args {
		data, err := ioutil.ReadFile(name)
		if err != nil {
			log.Fatal(err)
		}
		goProg := do(name, data)

		if *flagW {
			out := strings.TrimSuffix(filepath.Base(name), ".c") + ".go"
			err := ioutil.WriteFile(out, goProg, 0666)
			if err != nil {
				log.Fatal(err)
			}
			continue
		}
		os.Stdout.Write(goProg)
	}
}

func do(name string, data []byte) []byte {
	var types []string
	haveType := make(map[string]bool)
	var prog *cc.Prog
	for {
		p, err := cc.Read(name, bytes.NewReader(data), types)
		if err == nil {
			prog = p
			break
		}

		// Can we find some new inferred type names?
		n := len(haveType)
		if *verbose {
			log.Printf("parse errors:\n%s", err)
		}
		for _, line := range strings.Split(err.Error(), "\n") {
			prompts := []string{
				"syntax error near ",
				"invalid function definition for ",
				"likely type near ",
			}
			for _, p := range prompts {
				if i := strings.Index(line, p); i >= 0 {
					word := line[i+len(p):]
					if !haveType[word] {
						haveType[word] = true
						if *verbose {
							log.Printf("assume %s is type", word)
						}
						types = append(types, word)
					}
					break
				}
			}
		}
		if len(haveType) == n {
			log.Fatal(err)
		}
	}

	rewriteSyntax(prog)
	simplifyBool(prog)
	decls := renameDecls(prog)
	moveDecls(decls)
	return writeGo(prog, decls)
}

func writeGo(prog *cc.Prog, decls []*cc.Decl) []byte {
	p := new(Printer)
	p.Package = "my/pkg"

	if len(decls) > 0 {
		steal := 0
		for i, com := range decls[0].Comments.Before {
			if com.Text == "" {
				steal = i + 1
			}
		}
		prog.Comments.Before = append(prog.Comments.Before, decls[0].Comments.Before[:steal]...)
		decls[0].Comments.Before = decls[0].Comments.Before[steal:]
	} else {
		steal := 0
		for i, com := range prog.Comments.After {
			if com.Text == "" {
				steal = i + 1
			}
			if com.Directive {
				break
			}
		}
		prog.Comments.Before = append(prog.Comments.Before, prog.Comments.After[:steal]...)
		prog.Comments.After = prog.Comments.After[steal:]
	}

	p.Print(prog.Comments.Before)

	for len(decls) > 0 && decls[0].Blank {
		p.Print(decls[0], Newline)
		decls = decls[1:]
	}

	pkg := path.Base(p.Package)
	p.Print("package ", pkg, "\n\n")

	for _, d := range decls {
		p.printDecl(d, true)
		p.Print(Newline)
	}
	p.Print(prog.Comments.After)

	buf := p.Bytes()

	if len(rules) > 0 {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "output", buf, parser.ParseComments)
		if err != nil {
			log.Printf("parsing Go for %s before rewrites: %v", prog.Span.Start.File, err)
			return buf
		}
		f = rewriteFile(fset, f, rules)
		var out bytes.Buffer
		if err := format.Node(&out, fset, f); err != nil {
			log.Fatalf("reformatting %s after rewrites: %v", prog.Span.Start.File, err)
		}
		buf = out.Bytes()
	}

	if buf1, err := format.Source(buf); err == nil {
		buf = buf1
	}
	return buf
}
