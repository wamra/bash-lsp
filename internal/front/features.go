package front

import (
	"sort"

	"github.com/wamra/bash-lsp/internal/front/syntax"

	"github.com/wamra/bash-lsp/tree"
)

// The lowerer records syntactic evidence of a dialect or a bash release as it
// lowers (tree.File.Features): an operator or keyword the parser read, never
// text that only looks like one (a string, a quoted heredoc, a comment).
// Nothing is walked a second time. The recording is by token, so an
// expansion inside an unquoted heredoc counts and a quoted heredoc's text
// does not.

// featureLen records key over n bytes from pos.
func (l *lowerer) featureLen(key string, pos syntax.Pos, n int) {
	if !pos.IsValid() || pos.IsRecovered() {
		return
	}
	start := l.pos(pos)
	l.addFeature(key, start.Offset, start.Offset+n)
}

// featureFrom records key over [a, b).
func (l *lowerer) featureFrom(key string, a, b syntax.Pos) {
	if !a.IsValid() || !b.IsValid() || a.IsRecovered() || b.IsRecovered() {
		return
	}
	l.addFeature(key, l.pos(a).Offset, l.pos(b).Offset)
}

// featureNode records key over the range of a lowered node.
func (l *lowerer) featureNode(key string, n *tree.Node) {
	l.feats = append(l.feats, tree.Feature{Key: key, Range: n.Range})
}

func (l *lowerer) addFeature(key string, a, b int) {
	src := len(l.f.Src)
	if a < 0 || a > src {
		return
	}
	b = min(max(b, a), src)
	l.feats = append(l.feats, tree.Feature{Key: key, Range: tree.Range{Start: l.f.PosAt(a), End: l.f.PosAt(b)}})
}

// pipeOp records a `|&` operator.
func (l *lowerer) pipeOp(b *syntax.BinaryCmd) {
	if b.Op == syntax.PipeAll {
		l.featureLen(tree.FeatPipeAll, b.OpPos, 2)
	}
}

// paramFeatures records what a lowered parameter expansion n (from p) shows:
// case modification, a transformation, a negative subscript.
func (l *lowerer) paramFeatures(p *syntax.ParamExp, n *tree.Node) {
	if p.Exp != nil {
		switch p.Exp.Op {
		case syntax.UpperFirst, syntax.UpperAll, syntax.LowerFirst, syntax.LowerAll:
			l.featureNode(tree.FeatCaseModification, n)
		case syntax.OtherParamOps:
			l.featureNode(tree.FeatTransformOp, n)
		}
	}
	if negativeIndex(p.Index) {
		l.featureNode(tree.FeatNegativeSubscript, n)
	}
}

// negativeIndex reports whether a subscript is a minus sign before an
// integer literal (`[-1]`).
func negativeIndex(e syntax.ArithmExpr) bool {
	u, ok := e.(*syntax.UnaryArithm)
	if !ok || u.Op != syntax.Minus || u.Post {
		return false
	}
	w, ok := u.X.(*syntax.Word)
	if !ok || len(w.Parts) != 1 {
		return false
	}
	lit, ok := w.Parts[0].(*syntax.Lit)
	if !ok || lit.Value == "" {
		return false
	}
	for i := 0; i < len(lit.Value); i++ {
		if lit.Value[i] < '0' || lit.Value[i] > '9' {
			return false
		}
	}
	return true
}

// sortFeatures puts features in source order. The lowerer reaches some of
// them out of order (arithmetic and test operands are flattened, the
// operators of a chain are met before their operands), and ties keep the
// order they were found in.
func sortFeatures(fs []tree.Feature) {
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].Range.Start.Offset < fs[j].Range.Start.Offset })
}
