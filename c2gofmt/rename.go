// Copyright 2015 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"rsc.io/cmd/c2gofmt/internal/cc"
)

var goKeyword = map[string]bool{
	"chan":        true,
	"defer":       true,
	"fallthrough": true,
	"func":        true,
	"go":          true,
	"import":      true,
	"interface":   true,
	"iota":        true,
	"map":         true,
	"package":     true,
	"range":       true,
	"select":      true,
	"type":        true,
	"var":         true,

	// not keywords but still need renaming
	"fmt":   true,
	"path":  true,
	"rune":  true,
	"true":  true,
	"false": true,
}

// renameDecls renames file-local declarations to make them
// unique across the whole set of files being compiled.
// For now, it appends the file base name to the declared name.
// Eventually it could be smarter and not do that when not necessary.
// It also renames names like 'type' and 'func' to avoid Go keywords.
//
// renameDecls returns the new list of top-level declarations.
func renameDecls(prog *cc.Prog) []*cc.Decl {
	// Rewrite C identifiers to avoid important Go words (keywords, iota, etc).
	cc.Preorder(prog, func(x cc.Syntax) {
		switch x := x.(type) {
		case *cc.Decl:
			if goKeyword[x.Name] {
				// NOTE: Must put _ last so that name can be upper-cased for export.
				x.Name += "_"
			}

		case *cc.Stmt:
			for _, lab := range x.Labels {
				if goKeyword[lab.Name] {
					lab.Name += "_"
				}
			}
			switch x.Op {
			case cc.Goto:
				if goKeyword[x.Text] {
					x.Text += "_"
				}
			}

		case *cc.Expr:
			switch x.Op {
			case cc.Dot, cc.Arrow, cc.Name:
				if goKeyword[x.Text] {
					x.Text += "_"
				}
			}
		}
	})

	// Build list of declared top-level names.
	// Not just prog.Decls because of enums and struct definitions.
	typedefs := map[*cc.Type]*cc.Decl{}
	for _, d := range prog.Decls {
		if d.Storage&cc.Typedef != 0 {
			typedefs[d.Type] = d
		}
	}

	var decls []*cc.Decl
	for _, d := range prog.Decls {
		if d.Blank {
			decls = append(decls, d)
			continue
		}
		if d.Name == "" {
			if td := typedefs[d.Type]; td != nil {
				// Print the definition here and not at the location of the typedef.
				td.Blank = true
				d.Name = td.Name
				d.Storage |= cc.Typedef
				decls = append(decls, d)
				continue
			}
			switch d.Type.Kind {
			case cc.Struct:
				if d.Type.Tag != "" {
					decls = append(decls, d)
					d.Name = d.Type.Tag
					d.Storage = cc.Typedef
				} else {
					d.Blank = true
					decls = append(decls, d)
				}
				if d.Type.TypeDecl == nil {
					d.Type.TypeDecl = d
				}
			case cc.Enum:
				d.Blank = true
				decls = append(decls, d)
				for _, dd := range d.Type.Decls {
					decls = append(decls, dd)
				}
			}
			continue
		}
		decls = append(decls, d)
		if d.Storage&cc.Typedef != 0 && d.Type != nil && d.Type.TypeDecl == nil {
			d.Type.TypeDecl = d
		}
	}

	// Identify declaration conflicts.
	count := make(map[string]int)
	src := make(map[string]string)
	for _, d := range decls {
		if d.Blank {
			continue
		}
		if count[d.Name]++; count[d.Name] > 1 {
			fprintf(d.Span, "conflicting name %s (last at %s)", d.Name, src[d.Name])
			continue
		}
		src[d.Name] = fmt.Sprintf("%s:%d", d.Span.Start.File, d.Span.Start.Line)
	}

	// Rename static, conflicting names.
	for _, d := range decls {
		if count[d.Name] > 1 {
			file := filepath.Base(d.Span.Start.File)
			if i := strings.Index(file, "."); i >= 0 {
				file = file[:i]
			}
			d.Name += "_" + file
		}

		if d.Type != nil && d.Type.Kind == cc.Func {
			if d.Body != nil {
				for _, s := range d.Body.Block {
					if s.Op == cc.StmtDecl && s.Decl.Storage&cc.Static != 0 {
						// Add function name as prefix.
						// Will print at top level.
						dd := s.Decl
						dd.Name = d.Name + "_" + dd.Name
					}
				}
			}
		}
	}

	return decls
}
