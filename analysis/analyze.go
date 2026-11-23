package analysis

import (
	"fmt"

	"github.com/wamra/bash-lsp/parse"
	"github.com/wamra/bash-lsp/tree"
)

// MaxFileBytes is the largest file Analyze parses; a larger one is included
// as a skipped file. The workspace uses the same limit for files on disk and
// open documents.
const MaxFileBytes = 2 << 20

// Analyze parses and binds every entry of files (keys are absolute paths),
// resolves `source` commands among them and returns generation 1 of the
// snapshot. A file over MaxFileBytes is part of the snapshot with no
// statements and File.Skipped set. opt.Prev is honoured as in Build.
func Analyze(files map[string][]byte, opt Options) *Snapshot {
	facts := make(map[string]*FileFacts, len(files))
	for path, src := range files {
		if len(src) > MaxFileBytes {
			f := tree.NewFile(path, nil)
			f.Dialect = parse.Detect(path, src)
			f.Skipped = fmt.Sprintf("file is %d bytes, over the %d byte limit", len(src), MaxFileBytes)
			facts[path] = Bind(f)
			continue
		}
		facts[path] = Bind(parse.File(path, src))
	}
	return Build(facts, opt, 1)
}
