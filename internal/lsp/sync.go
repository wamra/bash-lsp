package lsp

import "github.com/wamra/bash-lsp/tree"

// applyChange applies one content change to text. A change without a range
// replaces the text. Ranged edits are in UTF-16 units; positions outside the
// text clamp (a column past the line end to the line end, a line past the
// last to the end of the text).
func applyChange(text []byte, ch contentChange) []byte {
	if ch.Range == nil {
		return []byte(ch.Text)
	}
	f := tree.NewFile("", text)
	start := f.FromUTF16(ch.Range.Start.Line, ch.Range.Start.Character).Offset
	end := f.FromUTF16(ch.Range.End.Line, ch.Range.End.Character).Offset
	if end < start {
		end = start
	}
	out := make([]byte, 0, len(text)-(end-start)+len(ch.Text))
	out = append(out, text[:start]...)
	out = append(out, ch.Text...)
	return append(out, text[end:]...)
}

// applyChanges applies changes in order.
func applyChanges(text []byte, changes []contentChange) []byte {
	for _, ch := range changes {
		text = applyChange(text, ch)
	}
	return text
}
