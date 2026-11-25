package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/query"
	"github.com/wamra/bash-lsp/internal/redact"
)

const renameUsage = "usage: bash-lsp rename SYMBOL|FILE:LINE:COL NEW [--preview|--apply] [--json] [--root DIR] [--]\n" +
	"  SYMBOL is a function name; preview (the default) writes nothing; --apply rewrites the files.\n" +
	"  exit 0 done, 1 failure (a partial apply included), 2 usage, 4 refused"

// Exit codes of rename beyond the common ones.
const exitRefused = 4

// renameHooks let a test change the disk between the steps of an apply.
type renameHooks struct {
	afterPlan    func() // the plan is built, nothing is checked yet
	afterChecks  func() // every file passed its checks, nothing is staged yet
	afterStaging func() // every file is staged, none is checked again
	beforeRename func() // every file is staged and checked again, none replaced
}

var applyHooks = &renameHooks{}

func (h *renameHooks) run(f func()) {
	if f != nil {
		f()
	}
}

type renameOpts struct {
	spec, newName string
	apply, json   bool
	root          string
}

func parseRenameArgs(args []string) (renameOpts, error) {
	o := renameOpts{root: "."}
	var pos []string
	preview := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, inline, hasInline := strings.Cut(a, "=")
		switch {
		case a == "--":
			pos = append(pos, args[i+1:]...)
			i = len(args)
		case a == "--json":
			o.json = true
		case a == "--apply":
			o.apply = true
		case a == "--preview":
			preview = true
		case name == "--root":
			switch {
			case hasInline:
				o.root = inline
			case i+1 < len(args):
				i++
				o.root = args[i]
			default:
				return o, errors.New("--root needs a value")
			}
		case strings.HasPrefix(a, "-") && a != "-":
			return o, fmt.Errorf("unknown flag %q for rename (put a name that starts with - after --)", a)
		default:
			pos = append(pos, a)
		}
	}
	if o.apply && preview {
		return o, errors.New("--preview and --apply exclude each other")
	}
	if len(pos) != 2 {
		return o, errors.New("rename wants a symbol and a new name")
	}
	o.spec, o.newName = pos[0], pos[1]
	return o, nil
}

// cmdRename plans the rename of a function or variable and prints the plan;
// with --apply it then rewrites the files. The plan is built from the snapshot
// of the workspace; nothing is written unless every file still holds the bytes
// the plan was built from.
func cmdRename(args []string, stdout, stderr io.Writer) int {
	o, err := parseRenameArgs(args)
	if err != nil {
		return usageErrorOf(stderr, renameUsage, err.Error())
	}
	ws, err := openWorkspace(o.root, stderr)
	if err != nil {
		return fail(stderr, err)
	}
	snap, root := ws.Snapshot(), ws.Root()
	plan, err := query.PlanRename(snap, root, o.spec, o.newName)
	if err != nil {
		return failRename(stderr, err)
	}
	doc := query.RenameDocOf(snap, root, plan, 0)
	var wrote []string
	if o.apply {
		applyHooks.run(applyHooks.afterPlan)
		if wrote, err = applyRename(snap, root, plan); err != nil {
			return failRename(stderr, err)
		}
	}
	if o.json {
		b, err := query.Marshal(doc)
		if err != nil {
			return fail(stderr, err)
		}
		stdout.Write(redact.JSON(b))
		return 0
	}
	for _, l := range renameLines(doc) {
		fmt.Fprintln(stdout, redact.String(l))
	}
	for _, w := range wrote {
		fmt.Fprintf(stdout, "wrote  %s\n", w)
	}
	if doc.Truncated {
		fmt.Fprintf(stderr, "bash-lsp: the answer was cut (at most %d records per list); --json is cut the same way\n", query.MaxResults)
	}
	return 0
}

// failRename reports an error of the rename: a refusal is exit 4 and prints
// its text as is, a partial write exit 1 with the files of both kinds named,
// anything else exit 1.
func failRename(stderr io.Writer, err error) int {
	var ref *query.RenameRefusedError
	var part *applyFailure
	switch {
	case errors.As(err, &ref):
		fmt.Fprintln(stderr, ref.Error())
		return exitRefused
	case errors.As(err, &part):
		fmt.Fprintf(stderr, "bash-lsp: rename: %v\n", part.err)
		if len(part.written) > 0 {
			fmt.Fprintf(stderr, "written: %s\n", strings.Join(part.written, ", "))
		}
		fmt.Fprintf(stderr, "not written: %s\n", strings.Join(part.notWritten, ", "))
		return 1
	}
	return failQuery(stderr, err)
}

// renameLines is the text form of a plan.
func renameLines(d *query.RenameDoc) []string {
	oneLine := strings.NewReplacer("\r", `\r`, "\n", `\n`)
	var out []string
	for _, e := range d.Edits {
		out = append(out, fmt.Sprintf("edit  %s:%d:%d  %s -> %s", e.Path, e.Line, e.Col, d.Old, d.New))
	}
	for _, x := range d.Excluded {
		out = append(out, fmt.Sprintf("skip  %s:%d:%d  %s/%s  %s", x.Path, x.Line, x.Col, x.Resolution, x.Reason, oneLine.Replace(x.Text)))
	}
	for _, o := range d.Others {
		out = append(out, fmt.Sprintf("other %s:%d:%d  different symbol named %s, not renamed", o.Path, o.Line, o.Col, d.Old))
	}
	for _, w := range d.Warnings {
		out = append(out, "warn  "+oneLine.Replace(w))
	}
	return out
}

// --- apply ---

// applyFailure is an apply that stopped after the checks: which files were
// replaced and which were not.
type applyFailure struct {
	err                 error
	written, notWritten []string
}

func (e *applyFailure) Error() string { return e.err.Error() }
func (e *applyFailure) Unwrap() error { return e.err }

// maxApplyGrowth is how much longer than the snapshot's text a file may be
// read before it is called changed.
const maxApplyGrowth = 1 << 16

type renameFile struct {
	path, rel string
	src       []byte
	edits     []analysis.RenameEdit
	out       []byte
	tmp, dest string
}

func refused(code, format string, args ...any) error {
	return &query.RenameRefusedError{Code: code, Reason: fmt.Sprintf(format, args...)}
}

// readChecked reads the file at f.path and checks it against what the plan
// was built from. The order matters for the code of the refusal: where the
// file is, what it is, whether it can be written, then its bytes.
func (f *renameFile) readChecked(root string) error {
	if _, ok := query.Inside(root, f.path); !ok {
		return refused("outside-root", "%s resolves outside the root", f.rel)
	}
	fi, err := os.Lstat(f.path)
	if err != nil {
		return refused("stale-file", "%s cannot be read any more: %v", f.rel, err)
	}
	if !fi.Mode().IsRegular() {
		return refused("not-regular-file", "%s is not a regular file (a link, directory or device)", f.rel)
	}
	if err := writable(f.path); err != nil {
		return refused("read-only", "%s cannot be written: %v", f.rel, err)
	}
	if fi.Size() > int64(len(f.src))+maxApplyGrowth {
		return refused("stale-file", "%s changed since the plan was built", f.rel)
	}
	b, err := os.ReadFile(f.path)
	if err != nil {
		return refused("stale-file", "%s cannot be read any more: %v", f.rel, err)
	}
	switch {
	case bytes.IndexByte(b, 0) >= 0:
		return refused("nul-byte", "%s holds a NUL byte", f.rel)
	case !utf8.Valid(b):
		return refused("invalid-utf8", "%s is not valid UTF-8", f.rel)
	case !bytes.Equal(b, f.src):
		return refused("stale-file", "%s changed since the plan was built", f.rel)
	}
	return nil
}

// build applies the edits to the plan's text of the file.
func (f *renameFile) build(old string) error {
	out := make([]byte, 0, len(f.src))
	at := 0
	for _, e := range f.edits {
		a, b := e.Range.Start.Offset, e.Range.End.Offset
		if a < at || b < a || b > len(f.src) || string(f.src[a:b]) != old {
			return fmt.Errorf("%s: the edit at byte %d does not cover %q", f.rel, a, old)
		}
		out = append(out, f.src[at:a]...)
		out = append(out, e.NewText...)
		at = b
	}
	f.out = append(out, f.src[at:]...)
	return nil
}

// applyRename writes the plan. Every file is checked first (inside the root
// once links are resolved, a regular file and not a link, writable, no NUL
// byte, valid UTF-8, the bytes of the snapshot); a failure refuses the whole
// rename and writes nothing. Then every new content is written to a temporary
// file beside its target, the files are checked once more, and only then are
// they renamed over their targets in path order. A failure while renaming
// leaves the files before it replaced; the error says which. It returns the
// root-relative names of the files it replaced.
func applyRename(snap *analysis.Snapshot, root string, plan *analysis.RenamePlan) ([]string, error) {
	var files []*renameFile
	b := query.NewBuilder(snap, root)
	for _, e := range plan.Edits {
		if n := len(files); n > 0 && files[n-1].path == e.Path {
			files[n-1].edits = append(files[n-1].edits, e)
			continue
		}
		ff := snap.Facts(e.Path)
		if ff == nil || ff.File == nil {
			return nil, fmt.Errorf("%s: not in the snapshot", b.Rel(e.Path))
		}
		files = append(files, &renameFile{path: e.Path, rel: b.Rel(e.Path), src: ff.File.Src, edits: []analysis.RenameEdit{e}})
	}
	for _, f := range files {
		if err := f.readChecked(root); err != nil {
			return nil, err
		}
	}
	for _, f := range files {
		if err := f.build(plan.Old); err != nil {
			return nil, err
		}
	}
	applyHooks.run(applyHooks.afterChecks)
	names := func(fs []*renameFile) []string {
		var out []string
		for _, f := range fs {
			out = append(out, f.rel)
		}
		return out
	}
	dropTemps := func(fs []*renameFile) {
		for _, f := range fs {
			if f.tmp != "" {
				os.Remove(f.tmp)
				f.tmp = ""
			}
		}
	}
	for _, f := range files {
		tmp, dest, err := stageFile(f.path, f.out)
		if err != nil {
			dropTemps(files)
			return nil, &applyFailure{err: fmt.Errorf("%s: %v", f.rel, err), notWritten: names(files)}
		}
		f.tmp, f.dest = tmp, dest
	}
	applyHooks.run(applyHooks.afterStaging)
	for _, f := range files {
		if err := f.readChecked(root); err != nil {
			dropTemps(files)
			return nil, err
		}
	}
	applyHooks.run(applyHooks.beforeRename)
	for i, f := range files {
		if err := os.Rename(f.tmp, f.dest); err != nil {
			dropTemps(files[i:])
			return nil, &applyFailure{err: fmt.Errorf("%s: %v", f.rel, err), written: names(files[:i]), notWritten: names(files[i:])}
		}
		f.tmp = ""
	}
	return names(files), nil
}
