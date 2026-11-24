package tree

// Feature is syntactic evidence that a file uses a construct tied to a
// shell dialect or a bash release: Key names the construct and Range holds
// the token that shows it (an operator, a keyword, or the whole expansion).
type Feature struct {
	Key   string
	Range Range
}

// The feature keys the parser front records. Each is also a row of the
// bash version table or the bash-only list of the diagnostics.
const (
	FeatCaseModification  = "case-modification"  // ${v,,} ${v^^} ${v,} ${v^}
	FeatCaseFallthrough   = "case-fallthrough"   // ;& and ;;&
	FeatPipeAll           = "pipe-all"           // |&
	FeatAppendAll         = "append-all"         // &>>
	FeatCoproc            = "coproc"             // coproc
	FeatNegativeSubscript = "negative-subscript" // ${a[-1]} and a[-1]=x
	FeatTransformOp       = "transform-op"       // ${v@Q}
	FeatDoubleBracket     = "double-bracket"     // [[
	FeatFunctionKeyword   = "function-keyword"   // function f
	FeatArrayAssign       = "array-assign"       // a=(1)
	FeatAnsiCQuote        = "ansi-c-quote"       // $'x'
	FeatHerestring        = "herestring"         // <<<
	FeatRedirectAll       = "redirect-all"       // &>
	FeatProcessSubst      = "process-subst"      // <( and >(
	FeatSourceBuiltin     = "source-builtin"     // source
)
