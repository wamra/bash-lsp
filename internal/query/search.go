package query

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/extool"
	"github.com/wamra/bash-lsp/internal/redact"
)

const (
	// NativeLimit is how many symbols Search takes from the native name match.
	NativeLimit = 50
	// CodeIndexTimeout and CodeIndexMaxOutput bound one code-index run.
	CodeIndexTimeout   = 10 * time.Second
	CodeIndexMaxOutput = 1 << 20
	// MaxText is the longest search text Search accepts, in bytes.
	MaxText = 4096

	maxReason = 200
)

// CodeIndex is what the code-index program contributed to a search: Used says
// whether its answer is in Hits; when it is not, Reason says why. Hits are
// code-index's own records, passed through unchanged.
type CodeIndex struct {
	Used   bool              `json:"used"`
	Reason string            `json:"reason"`
	Hits   []json.RawMessage `json:"hits"`
}

// SearchDoc is the answer of Search.
type SearchDoc struct {
	Generation uint64    `json:"generation"`
	Native     []Symbol  `json:"native"`
	CodeIndex  CodeIndex `json:"codeindex"`
	Truncated  bool      `json:"truncated,omitempty"`
}

// Len is the number of code-index hits; Cut returns a copy with k of them
// and "truncated": true. See Doc.Len.
func (d *SearchDoc) Len() int { return len(d.CodeIndex.Hits) }

// Cut is described at Len.
func (d *SearchDoc) Cut(k int) any {
	c := *d
	if k < len(c.CodeIndex.Hits) {
		c.CodeIndex.Hits, c.Truncated = c.CodeIndex.Hits[:k], true
	}
	return &c
}

// SearchArgs is the fixed command line of the code-index passthrough: the
// text is the one argument after "--", so no text can act as an option (not
// even "--mode=auto"), and --mode fts keeps the run off the network.
func SearchArgs(text string) []string {
	return []string{"search", "--mode", "fts", "--lang", "bash", "--k", "20", "--json", "--", text}
}

// IndexEnv is the environment variable code-index reads for its database.
const IndexEnv = "CODE_INDEX_DB"

// indexDB is the database code-index should open for root: the user's own
// $CODE_INDEX_DB when set, else <root>/.Wammy/code.sqlite (what code-index
// itself would find from a working directory inside root).
func indexDB(root string) string {
	if v := os.Getenv(IndexEnv); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			return abs
		}
		return v
	}
	return filepath.Join(root, ".Wammy", "code.sqlite")
}

// Search matches text against symbol names natively and, when the code-index
// program can be found, runs its full-text search over the index. The program
// is chosen by extool.Find (the configured path or PATH from prof, never one
// inside root), started through the extool runner with the filtered
// environment, no shell, and the neutral working directory: the index is
// named by CODE_INDEX_DB, so the process never starts inside the workspace.
// A failure of code-index is not an error of the search: CodeIndex.Used is
// false and Reason holds the first line it wrote to stderr (200 bytes at
// most, redacted).
func Search(ctx context.Context, s *analysis.Snapshot, root, text string, cfg config.Settings, prof *extool.Profile) *SearchDoc {
	doc := &SearchDoc{Generation: s.Generation(), Native: []Symbol{}, CodeIndex: CodeIndex{Hits: []json.RawMessage{}}}
	if len(text) > MaxText {
		doc.CodeIndex.Reason = fmt.Sprintf("search text is longer than %d bytes", MaxText)
		return doc
	}
	if d, ok := FindSymbol(s, root, text, NativeLimit).(*Doc); ok {
		if syms, ok := d.Results.([]Symbol); ok {
			doc.Native = syms
		}
	}
	tool, ref, ok := extool.Find("code-index", cfg.CodeIndexPath, prof, []string{root})
	switch {
	case ref != nil:
		doc.CodeIndex.Reason = clip(fmt.Sprintf("code-index at %s is not used: %s", ref.Path, ref.Reason))
		return doc
	case !ok:
		doc.CodeIndex.Reason = "code-index was not found on PATH"
		return doc
	}
	res, err := extool.Run(ctx, tool, SearchArgs(text), extool.RunOptions{
		Dir:       extool.NeutralDir(),
		Env:       extool.ChildEnv([]string{root}, IndexEnv+"="+indexDB(root)),
		Timeout:   CodeIndexTimeout,
		MaxOutput: CodeIndexMaxOutput,
	})
	switch {
	case err != nil:
		doc.CodeIndex.Reason = clip(err.Error())
	case res.Exit != 0:
		line := firstLine(res.Stderr)
		if line == "" {
			line = fmt.Sprintf("exit status %d", res.Exit)
		}
		doc.CodeIndex.Reason = clip(line)
	default:
		var out struct {
			Hits []json.RawMessage `json:"hits"`
		}
		if err := json.Unmarshal(res.Stdout, &out); err != nil {
			doc.CodeIndex.Reason = clip("code-index output is not the expected JSON: " + err.Error())
			break
		}
		if out.Hits != nil {
			doc.CodeIndex.Hits = out.Hits
		}
		doc.CodeIndex.Used = true
	}
	return doc
}

// firstLine is the first non-empty line of b.
func firstLine(b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// clip redacts s and cuts it to maxReason bytes at a rune boundary.
func clip(s string) string {
	s = redact.String(s)
	if len(s) > maxReason {
		s = s[:maxReason]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// hitLine is the text form of one code-index hit.
func hitLine(raw json.RawMessage) string {
	var h struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return string(raw)
	}
	return fmt.Sprintf("%s:%d  %s  %s", h.Path, h.Line, h.Kind, h.Name)
}

// Lines is the text form of the answer.
func (d *SearchDoc) Lines() []string {
	var out []string
	for _, s := range d.Native {
		out = append(out, fmt.Sprintf("%s:%d:%d  %s  %s", s.Path, s.Line, s.Col, s.Kind, s.Name))
	}
	if d.CodeIndex.Used {
		out = append(out, "code-index:")
		for _, h := range d.CodeIndex.Hits {
			out = append(out, "  "+hitLine(h))
		}
	} else {
		out = append(out, "code-index: not used: "+d.CodeIndex.Reason)
	}
	return out
}
