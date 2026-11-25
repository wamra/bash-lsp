package query

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/redact"
)

// maxRenameText is the most bytes of source text an excluded site or a
// warning shows.
const (
	maxRenameText    = 200
	maxRenameWarning = 400
)

// RenameSkip is a site that mentions the symbol and is not edited: how the
// analysis resolved it, why it is left alone, and the text there (credentials
// redacted, then cut to 200 bytes at a rune boundary).
type RenameSkip struct {
	Site
	Resolution string `json:"resolution"`
	Reason     string `json:"reason"`
	Text       string `json:"text"`
}

// RenameDoc is the plan of a rename that was not refused: the edits, the
// sites left alone and why, other symbols of the same name that stay, and
// what the analysis could not see. Lists are cut at the caller's maximum and
// the document then says "truncated": true.
type RenameDoc struct {
	Generation uint64       `json:"generation"`
	Symbol     string       `json:"symbol"`
	Old        string       `json:"old"`
	New        string       `json:"new"`
	Edits      []Site       `json:"edits"`
	Excluded   []RenameSkip `json:"excluded"`
	Others     []Site       `json:"others"`
	Warnings   []string     `json:"warnings"`
	Truncated  bool         `json:"truncated,omitempty"`
}

// Len is the length of the longest list; Cut cuts every list to k records
// (the MCP adapter holds a reply to its size cap that way).
func (d *RenameDoc) Len() int {
	return max(len(d.Edits), len(d.Excluded), len(d.Others), len(d.Warnings))
}

// Cut is described at Len.
func (d *RenameDoc) Cut(k int) any {
	c := *d
	k = max(k, 0)
	if len(c.Edits) > k {
		c.Edits, c.Truncated = c.Edits[:k], true
	}
	if len(c.Excluded) > k {
		c.Excluded, c.Truncated = c.Excluded[:k], true
	}
	if len(c.Others) > k {
		c.Others, c.Truncated = c.Others[:k], true
	}
	if len(c.Warnings) > k {
		c.Warnings, c.Truncated = c.Warnings[:k], true
	}
	return &c
}

// RenameRefusedError is a rename the analysis refuses, or an apply that finds
// the files no longer match the plan. Error is the text both the command line
// and the MCP tool print: "refused: <code>: <reason>", then one "site  path:line:col"
// line per site the reason is about.
type RenameRefusedError struct {
	Code, Reason string
	Sites        []Site
}

func (e *RenameRefusedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "refused: %s: %s", e.Code, oneLine(redact.String(e.Reason)))
	for _, s := range e.Sites {
		fmt.Fprintf(&b, "\nsite  %s:%d:%d", s.Path, s.Line, s.Col)
	}
	return b.String()
}

// oneLine keeps a text on one line: a line break is written out.
func oneLine(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(s)
}

// clipRunes cuts s to n bytes at a rune boundary.
func clipRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// safeText redacts s and then cuts it: cutting first could leave half of a
// credential that no pattern recognises.
func safeText(s string, n int) string { return clipRunes(redact.String(s), n) }

// PlanRename plans the rename of spec, a function name or path:line:col, to
// newName. A refusal of the analysis is a *RenameRefusedError.
func PlanRename(s *analysis.Snapshot, root, spec, newName string) (*analysis.RenamePlan, error) {
	if err := CheckInside(root, spec); err != nil {
		return nil, err
	}
	var plan *analysis.RenamePlan
	path, off, ok, err := s.Position(spec)
	if err != nil {
		return nil, err
	}
	if ok {
		plan, err = s.PlanRename(path, off, newName)
	} else {
		plan, err = s.PlanRenameByName(spec, newName)
	}
	if err != nil {
		var r *analysis.RenameRefusal
		if errors.As(err, &r) {
			b := NewBuilder(s, root)
			out := &RenameRefusedError{Code: r.Code, Reason: r.Reason}
			for _, l := range r.Sites {
				out.Sites = append(out.Sites, b.Site(l))
			}
			return nil, out
		}
		return nil, err
	}
	return plan, nil
}

// RenameDocOf renders a plan, every list cut to max records (MaxResults when
// max is 0 or larger).
func RenameDocOf(s *analysis.Snapshot, root string, plan *analysis.RenamePlan, max int) *RenameDoc {
	b := NewBuilder(s, root)
	max = capMax(max)
	d := &RenameDoc{
		Generation: s.Generation(),
		Symbol:     kindName(plan.Symbol.Kind) + " " + plan.Symbol.Name,
		Old:        plan.Old,
		New:        plan.New,
		Edits:      []Site{},
		Excluded:   []RenameSkip{},
		Others:     []Site{},
		Warnings:   []string{},
	}
	var cut bool
	for _, e := range cutTo(plan.Edits, max, &cut) {
		d.Edits = append(d.Edits, b.Site(analysis.Location{Path: e.Path, Range: e.Range}))
	}
	for _, x := range cutTo(plan.Excluded, max, &cut) {
		d.Excluded = append(d.Excluded, RenameSkip{
			Site:       b.Site(x.Loc),
			Resolution: x.Res.String(),
			Reason:     x.Reason,
			Text:       safeText(x.Text, maxRenameText),
		})
	}
	for _, l := range cutTo(plan.Others, max, &cut) {
		d.Others = append(d.Others, b.Site(l))
	}
	for _, w := range cutTo(plan.Warnings, max, &cut) {
		d.Warnings = append(d.Warnings, safeText(w, maxRenameWarning))
	}
	d.Truncated = cut
	return d
}

func cutTo[T any](list []T, n int, cut *bool) []T {
	if len(list) > n {
		*cut = true
		return list[:n]
	}
	return list
}

// PreviewRename plans the rename and renders it; nothing is written.
func PreviewRename(s *analysis.Snapshot, root, spec, newName string, max int) (*RenameDoc, error) {
	plan, err := PlanRename(s, root, spec, newName)
	if err != nil {
		return nil, err
	}
	return RenameDocOf(s, root, plan, max), nil
}
