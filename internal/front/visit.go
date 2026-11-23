package front

import "mvdan.cc/sh/v3/syntax"

// visitAll calls f on every node under root, root included, in no particular
// order. It replaces syntax.Walk for the two whole-file scans (comments and
// unclosed quotes), for two reasons:
//
//   - Walk recurses once per tree level. A long operator chain the parser
//     accepts (`1+1+1...`, 900,000 deep in a 2 MiB file) overflowed the stack
//     in it, which is fatal.
//   - For a statement with comments, Walk calls Stmt.End, which descends
//     through every statement nested in a time, coproc, function or pipeline
//     chain: a chain of `f() # c` lines took seconds.
//
// visitAll keeps its own stack and never calls End. The cases follow
// syntax.Walk in mvdan.cc/sh v3.14.1, with one difference: Walk stops at the
// first comment that trails a statement or case item and skips any after it
// (`a) ;; # one` then `# two` before `esac`); visitAll visits them all.
func visitAll(root syntax.Node, f func(syntax.Node)) {
	stack := []syntax.Node{root}
	push := func(n syntax.Node) { stack = append(stack, n) }
	comments := func(cs []syntax.Comment) {
		for i := range cs {
			push(&cs[i])
		}
	}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if node == nil {
			continue
		}
		f(node)
		switch n := node.(type) {
		case *syntax.File:
			for _, s := range n.Stmts {
				push(s)
			}
			comments(n.Last)
		case *syntax.Stmt:
			comments(n.Comments)
			if n.Cmd != nil {
				push(n.Cmd)
			}
			for _, r := range n.Redirs {
				push(r)
			}
		case *syntax.Assign:
			if n.Name != nil {
				push(n.Name)
			}
			if n.Value != nil {
				push(n.Value)
			}
			if n.Index != nil {
				push(n.Index)
			}
			if n.Array != nil {
				push(n.Array)
			}
		case *syntax.Redirect:
			if n.N != nil {
				push(n.N)
			}
			if n.Word != nil {
				push(n.Word)
			}
			if n.Hdoc != nil {
				push(n.Hdoc)
			}
		case *syntax.CallExpr:
			for _, a := range n.Assigns {
				push(a)
			}
			for _, w := range n.Args {
				push(w)
			}
		case *syntax.Subshell:
			stmts(push, n.Stmts)
			comments(n.Last)
		case *syntax.Block:
			stmts(push, n.Stmts)
			comments(n.Last)
		case *syntax.IfClause:
			stmts(push, n.Cond)
			comments(n.CondLast)
			stmts(push, n.Then)
			comments(n.ThenLast)
			comments(n.Last)
			if n.Else != nil {
				push(n.Else)
			}
		case *syntax.WhileClause:
			stmts(push, n.Cond)
			comments(n.CondLast)
			stmts(push, n.Do)
			comments(n.DoLast)
		case *syntax.ForClause:
			if n.Loop != nil {
				push(n.Loop)
			}
			stmts(push, n.Do)
			comments(n.DoLast)
		case *syntax.WordIter:
			if n.Name != nil {
				push(n.Name)
			}
			for _, w := range n.Items {
				push(w)
			}
		case *syntax.CStyleLoop:
			push(n.Init)
			push(n.Cond)
			push(n.Post)
		case *syntax.BinaryCmd:
			push(n.X)
			push(n.Y)
		case *syntax.FuncDecl:
			if n.Name != nil {
				push(n.Name)
			}
			for _, nm := range n.Names {
				push(nm)
			}
			if n.Body != nil {
				push(n.Body)
			}
		case *syntax.Word:
			for _, p := range n.Parts {
				push(p)
			}
		case *syntax.DblQuoted:
			for _, p := range n.Parts {
				push(p)
			}
		case *syntax.CmdSubst:
			stmts(push, n.Stmts)
			comments(n.Last)
		case *syntax.ParamExp:
			if n.Flags != nil {
				push(n.Flags)
			}
			if n.Param != nil {
				push(n.Param)
			}
			if n.NestedParam != nil {
				push(n.NestedParam)
			}
			if n.Index != nil {
				push(n.Index)
			}
			if n.Slice != nil {
				push(n.Slice.Offset)
				push(n.Slice.Length)
			}
			if n.Repl != nil {
				if n.Repl.Orig != nil {
					push(n.Repl.Orig)
				}
				if n.Repl.With != nil {
					push(n.Repl.With)
				}
			}
			if n.Exp != nil && n.Exp.Word != nil {
				push(n.Exp.Word)
			}
		case *syntax.ArithmExp:
			push(n.X)
		case *syntax.ArithmCmd:
			push(n.X)
		case *syntax.BinaryArithm:
			push(n.X)
			push(n.Y)
		case *syntax.BinaryTest:
			push(n.X)
			push(n.Y)
		case *syntax.UnaryArithm:
			push(n.X)
		case *syntax.UnaryTest:
			push(n.X)
		case *syntax.ParenArithm:
			push(n.X)
		case *syntax.FlagsArithm:
			if n.Flags != nil {
				push(n.Flags)
			}
			push(n.X)
		case *syntax.ParenTest:
			push(n.X)
		case *syntax.CaseClause:
			if n.Word != nil {
				push(n.Word)
			}
			for _, it := range n.Items {
				push(it)
			}
			comments(n.Last)
		case *syntax.CaseItem:
			comments(n.Comments)
			for _, p := range n.Patterns {
				push(p)
			}
			stmts(push, n.Stmts)
			comments(n.Last)
		case *syntax.TestClause:
			push(n.X)
		case *syntax.DeclClause:
			for _, a := range n.Args {
				push(a)
			}
		case *syntax.ArrayExpr:
			for _, e := range n.Elems {
				push(e)
			}
			comments(n.Last)
		case *syntax.ArrayElem:
			comments(n.Comments)
			if n.Index != nil {
				push(n.Index)
			}
			if n.Value != nil {
				push(n.Value)
			}
		case *syntax.ExtGlob:
			if n.Pattern != nil {
				push(n.Pattern)
			}
		case *syntax.ProcSubst:
			stmts(push, n.Stmts)
			comments(n.Last)
		case *syntax.TimeClause:
			if n.Stmt != nil {
				push(n.Stmt)
			}
		case *syntax.CoprocClause:
			if n.Name != nil {
				push(n.Name)
			}
			if n.Stmt != nil {
				push(n.Stmt)
			}
		case *syntax.LetClause:
			for _, e := range n.Exprs {
				push(e)
			}
		case *syntax.TestDecl:
			if n.Description != nil {
				push(n.Description)
			}
			if n.Body != nil {
				push(n.Body)
			}
		}
	}
}

func stmts(push func(syntax.Node), list []*syntax.Stmt) {
	for _, s := range list {
		push(s)
	}
}
