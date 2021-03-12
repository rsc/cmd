// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "rsc.io/cmd/c2gofmt/internal/cc"

func moveDecls(progDecls []*cc.Decl) {
	for _, d := range progDecls {
		if d.Type != nil && d.Type.Kind == cc.Func && d.Body != nil {
			moveFuncDecls(d)
		}
	}
}

func inlineBlockNoBrace(x *cc.Stmt) {
	if x.Op == cc.Block {
		var list []*cc.Stmt
		for _, stmt := range x.Block {
			// keep stmt always, in case of labels, comments etc
			list = append(list, stmt)
			if stmt.Op == BlockNoBrace {
				list = append(list, stmt.Block...)
				stmt.Op = cc.Empty
				stmt.Block = nil
			}
		}
		x.Block = list
	}
}

func moveFuncDecls(fndecl *cc.Decl) {
	// Inline the BlockNoBraces into the Blocks, so that we can understand
	// the flow of the variables properly.
	cc.Postorder(fndecl.Body, func(x cc.Syntax) {
		switch x := x.(type) {
		case *cc.Stmt:
			inlineBlockNoBrace(x)
		}
	})

	// Push var declarations forward until we hit their uses.
	type usesVar struct {
		x cc.Syntax
		v *cc.Decl
	}
	uses := make(map[usesVar]bool)
	var decls []*cc.Decl
	cc.Preorder(fndecl.Body, func(x cc.Syntax) {
		if d, ok := x.(*cc.Decl); ok {
			decls = append(decls, d)
		}
	})
	copyUses := func(x, y cc.Syntax) {
		for _, d := range decls {
			if uses[usesVar{y, d}] {
				uses[usesVar{x, d}] = true
			}
		}
	}
	cc.Postorder(fndecl.Body, func(x cc.Syntax) {
		switch x := x.(type) {
		case *cc.Stmt:
			copyUses(x, x.Pre)
			copyUses(x, x.Expr)
			copyUses(x, x.Post)
			copyUses(x, x.Body)
			copyUses(x, x.Else)
			for _, y := range x.Block {
				copyUses(x, y)
			}
			copyUses(x, x.Decl)
		case *cc.Expr:
			if x.Op == cc.Name && x.XDecl != nil {
				uses[usesVar{x, x.XDecl}] = true
			}
			copyUses(x, x.Left)
			copyUses(x, x.Right)
			for _, y := range x.List {
				copyUses(x, y)
			}
			for _, y := range x.Block {
				copyUses(x, y)
			}
			for _, y := range x.After {
				copyUses(x, y)
			}
		case *cc.Decl:
			copyUses(x, x.Init)
		case *cc.Init:
			copyUses(x, x.Expr)
			for _, y := range x.Braced {
				copyUses(x, y)
			}
		}
	})

	anyUses := func(list []*cc.Stmt, d *cc.Decl) bool {
		for _, x := range list {
			if uses[usesVar{x, d}] {
				return true
			}
		}
		return false
	}

	addToBlock := func(x, decl *cc.Stmt) *cc.Stmt {
		if x.Op == cc.Block || x.Op == BlockNoBrace {
			x.Block = append([]*cc.Stmt{decl}, x.Block...)
			return x
		}
		return &cc.Stmt{
			Op:    cc.Block,
			Block: []*cc.Stmt{decl, x},
		}
	}

	var addToIf func(x, decl *cc.Stmt) bool
	addToIf = func(x, d *cc.Stmt) bool {
		if uses[usesVar{x.Pre, d.Decl}] || uses[usesVar{x.Expr, d.Decl}] {
			return false
		}
		if uses[usesVar{x.Body, d.Decl}] {
			x.Body = addToBlock(x.Body, d)
		}
		if uses[usesVar{x.Else, d.Decl}] {
			if x.Else.Op != cc.If || !addToIf(x.Else, d) {
				x.Else = addToBlock(x.Else, d)
			}
		}
		return true
	}

	cc.Preorder(fndecl.Body, func(x cc.Syntax) {
		switch x := x.(type) {
		case *cc.Stmt:
			if x.Op == cc.Block || x.Op == BlockNoBrace {
				out := x.Block[:0]
				var pending []*cc.Stmt // all StmtDecls
				for i, stmt := range x.Block {
					// Emit any required declarations.
					pendout := pending[:0]
					for _, d := range pending {
						if !uses[usesVar{stmt, d.Decl}] {
							pendout = append(pendout, d)
							continue
						}
						if stmt.Op == cc.StmtExpr && stmt.Expr.Op == cc.Eq && stmt.Expr.Left.Op == cc.Name && stmt.Expr.Left.XDecl == d.Decl {
							stmt.Expr.Op = ColonEq
							continue
						}
						if !anyUses(x.Block[i+1:], d.Decl) {
							switch stmt.Op {
							case cc.If:
								if addToIf(stmt, d) {
									continue
								}
							case cc.Block:
								addToBlock(stmt, d)
								continue
							case cc.For:
								if !uses[usesVar{stmt.Pre, d.Decl}] && !uses[usesVar{stmt.Expr, d.Decl}] && !uses[usesVar{stmt.Post, d.Decl}] {
									// Only used in body, and it is uninitialized on entry,
									// so it must be OK to use a fresh copy every time.
									stmt.Body = addToBlock(stmt.Body, d)
									continue
								}
								if stmt.Pre != nil && stmt.Pre.Op == cc.Eq && stmt.Pre.Left.Op == cc.Name && stmt.Pre.Left.XDecl == d.Decl {
									// Loop variable.
									stmt.Pre.Op = ColonEq
									continue
								}
							}
						}
						out = append(out, d)
					}
					pending = pendout

					// Pick up any uninitialized declarations for emitting later.
					if stmt.Op == cc.StmtDecl && stmt.Decl.Init == nil {
						pending = append(pending, stmt)
						// If the declaration is followed by a blank line,
						// as is the custom in C, remove it, to match the
						// custom in Go. Also, the var declaration is likely moving
						// so the blank line will not follow anything.
						if i+1 < len(x.Block) {
							if com := &x.Block[i+1].Comments; len(com.Before) > 0 && com.Before[0].Text == "" {
								com.Before = com.Before[1:]
							}
						}
						continue
					}
					out = append(out, stmt)
				}
				x.Block = out
			}
		}
	})
}
