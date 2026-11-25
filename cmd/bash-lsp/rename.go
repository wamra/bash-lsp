package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	afterPlan    func()                      // the plan is built, nothing is checked yet
	afterChecks  func()                      // every file passed its checks, nothing is staged yet
	afterStaging func()                      // every file is staged, none is checked again
	beforeRename func()                      // every file is staged and checked again, none replaced
	rename       func(from, to string) error // replaces os.Rename, for the files and for the roll back
}

func (h *renameHooks) move(from, to string) error {
	if h.rename != nil {
		return h.rename(from, to)
	}
	return os.Rename(from, to)
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

// renameResult is the JSON of `rename --apply` and of a rename that failed
// under --json: the plan when there is one, whether the files were written,
// and what went wrong.
type renameResult struct {
	*query.RenameDoc
	Applied    bool                      `json:"applied"`
	Written    []string                  `json:"written"`
	RolledBack []string                  `json:"rolled_back,omitempty"`
	NotWritten []string                  `json:"not_written,omitempty"`
	Refused    *query.RenameRefusedError `json:"refused,omitempty"`
	Error      string                    `json:"error,omitempty"`
}

func say(w io.Writer, format string, args ...any) {
	fmt.Fprint(w, redact.String(fmt.Sprintf(format, args...)))
}

func printJSON(w io.Writer, v any) int {
	b, err := query.Marshal(v)
	if err != nil {
		return 1
	}
	w.Write(redact.JSON(b))
	return 0
}

// cmdRename plans the rename of a function or variable and prints the plan;
// with --apply it then rewrites the files. The plan is built from the snapshot
// of the workspace; nothing is written unless every file still holds the bytes
// the plan was built from, and a failure while writing is rolled back.
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
		var ref *query.RenameRefusedError
		if o.json && errors.As(err, &ref) {
			printJSON(stdout, renameResult{Refused: ref})
		}
		return failRename(stderr, err)
	}
	doc := query.RenameDocOf(snap, root, plan, 0)
	res := renameResult{RenameDoc: doc, Written: []string{}}
	if o.apply {
		_, _, isPosition, _ := snap.Position(o.spec)
		if !isPosition && len(doc.Others) > 0 {
			// the name alone cannot tell which of the same-named symbols
			// was meant: the user said it by position, or nothing is written
			err = &query.RenameRefusedError{Code: "ambiguous-apply", Sites: doc.Others, Reason: fmt.Sprintf(
				"%d other symbols are also named %s and would stay; apply by path:line:col of the one to rename (preview by name shows them)", len(doc.Others), doc.Old)}
		} else {
			applyHooks.run(applyHooks.afterPlan)
			var wrote []string
			if wrote, err = applyRename(snap, root, plan); err == nil {
				res.Applied, res.Written = true, wrote
				if o.json {
					return printJSON(stdout, res)
				}
				printPlan(stdout, doc)
				for _, w := range wrote {
					say(stdout, "wrote  %s\n", w)
				}
				return cut(stderr, doc)
			}
		}
		// a failed apply still shows the plan
		var ref *query.RenameRefusedError
		var part *applyFailure
		switch {
		case errors.As(err, &part):
			res.Written, res.RolledBack, res.NotWritten = part.written, part.rolledBack, part.notWritten
			res.Error = redact.String(part.err.Error())
		case errors.As(err, &ref):
			res.Refused = ref
		default:
			res.Error = redact.String(err.Error())
		}
		if res.Written == nil {
			res.Written = []string{}
		}
		if o.json {
			printJSON(stdout, res)
		} else {
			printPlan(stdout, doc)
		}
		return failRename(stderr, err)
	}
	if o.json {
		return printJSON(stdout, doc)
	}
	printPlan(stdout, doc)
	return cut(stderr, doc)
}

func printPlan(stdout io.Writer, doc *query.RenameDoc) {
	for _, l := range renameLines(doc) {
		say(stdout, "%s\n", l)
	}
}

func cut(stderr io.Writer, doc *query.RenameDoc) int {
	if doc.Truncated {
		fmt.Fprintf(stderr, "bash-lsp: the answer was cut (at most %d records per list); --json is cut the same way\n", query.MaxResults)
	}
	return 0
}

// failRename reports an error of the rename on standard error: a refusal is
// exit 4 and prints its text; a failed write exit 1 with the files of each
// kind named (and, when the roll back did not finish, the command that
// reverses what is left); anything else exit 1.
func failRename(stderr io.Writer, err error) int {
	var ref *query.RenameRefusedError
	var part *applyFailure
	switch {
	case errors.As(err, &part):
		say(stderr, "bash-lsp: rename: %v\n", part.err)
		if len(part.rolledBack) > 0 {
			say(stderr, "rolled back: %s\n", strings.Join(part.rolledBack, ", "))
		}
		if len(part.written) > 0 {
			say(stderr, "written: %s\n", strings.Join(part.written, ", "))
			say(stderr, "the roll back did not finish: these files still hold the new name. To undo it: %s\n", part.reverse)
		}
		say(stderr, "not written: %s\n", strings.Join(part.notWritten, ", "))
		return 1
	case errors.As(err, &ref):
		say(stderr, "%s\n", ref.Error())
		return exitRefused
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
		// line breaks are written out first, then the text is cut again: the
		// line stays one line and stays under 200 bytes of text
		text := oneLine.Replace(x.Text)
		if len(text) > 200 {
			text = strings.TrimRight(cutBytes(text, 200), `\`)
		}
		out = append(out, fmt.Sprintf("skip  %s:%d:%d  %s/%s  %s", x.Path, x.Line, x.Col, x.Resolution, x.Reason, text))
	}
	for _, o := range d.Others {
		out = append(out, fmt.Sprintf("other %s:%d:%d  different symbol named %s, not renamed", o.Path, o.Line, o.Col, d.Old))
	}
	for _, w := range d.Warnings {
		out = append(out, "warn  "+oneLine.Replace(w))
	}
	return out
}

// cutBytes cuts s to n bytes on a rune boundary.
func cutBytes(s string, n int) string {
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// --- apply ---

// applyFailure is an apply that stopped after the checks. written are the
// files that still hold the new name (empty once the roll back finished),
// rolledBack the files put back, notWritten the files that hold their old
// text. reverse is the command that undoes a roll back that did not finish.
type applyFailure struct {
	err                             error
	written, rolledBack, notWritten []string
	reverse                         string
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
	fi        os.FileInfo
	tmp, dest string // the new text, staged beside dest
	back      string // the original text, staged beside dest, for the roll back
}

func refusedIssue(i *query.ApplyIssue) error {
	return &query.RenameRefusedError{Code: i.Code, Reason: i.Reason}
}

// check reads the file at f.path and checks it against what the plan was
// built from. The order matters for the code of the refusal: where the file
// is, what it is, whether it can be written, then its bytes.
func (f *renameFile) check(root string) error {
	fi, issue := query.CheckFileMeta(root, f.path, f.rel)
	if issue != nil {
		return refusedIssue(issue)
	}
	f.fi = fi
	if fi.Size() > int64(len(f.src))+maxApplyGrowth {
		return refusedIssue(&query.ApplyIssue{Code: query.IssueStale, Reason: f.rel + " changed since the plan was built"})
	}
	b, err := os.ReadFile(f.path)
	if err != nil {
		return refusedIssue(&query.ApplyIssue{Code: query.IssueStale, Reason: fmt.Sprintf("%s cannot be read any more: %v", f.rel, err)})
	}
	if issue := query.CheckFileBytes(f.rel, b, f.src); issue != nil {
		return refusedIssue(issue)
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

// stage writes data beside the file, keeps its owner, and returns the name.
func (f *renameFile) stage(data []byte) (string, error) {
	tmp, dest, err := stageFile(f.path, data)
	if err != nil {
		return "", err
	}
	f.dest = dest
	if err := query.PreserveOwner(tmp, f.fi); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("the owner of %s cannot be kept: %v", f.rel, err)
	}
	return tmp, nil
}

// applyRename writes the plan. Every file is checked first (inside the root
// once links are resolved, a regular file and not a link, no setuid, setgid
// or sticky bit, one hard link, owned by the user, writable, in a directory
// that takes a new file, no NUL byte, valid UTF-8, the bytes of the
// snapshot); a failure refuses the whole rename and writes nothing. Then the
// new text and, for the roll back, the original text of every file are
// written to temporary files beside their targets; each file is checked once
// more right before its own rename, and the files are renamed over their
// targets in path order. When a rename fails, the files already replaced are
// put back from their staged originals; the failure says which files were put
// back and which, if the roll back itself failed, still hold the new name,
// with the command that undoes them. Directories are synced after the
// renames. It returns the root-relative names of the files it replaced.
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
		if err := f.check(root); err != nil {
			return nil, err
		}
	}
	if err := checkDirs(files); err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := f.build(plan.Old); err != nil {
			return nil, err
		}
	}
	applyHooks.run(applyHooks.afterChecks)
	names := func(fs []*renameFile) []string {
		out := []string{}
		for _, f := range fs {
			out = append(out, f.rel)
		}
		return out
	}
	dropTemps := func() {
		for _, f := range files {
			if f.tmp != "" {
				os.Remove(f.tmp)
				f.tmp = ""
			}
			if f.back != "" {
				os.Remove(f.back)
				f.back = ""
			}
		}
	}
	for _, f := range files {
		var err error
		if f.tmp, err = f.stage(f.out); err == nil {
			f.back, err = f.stage(f.src)
		}
		if err != nil {
			dropTemps()
			return nil, &applyFailure{err: fmt.Errorf("%s: %v", f.rel, err), notWritten: names(files)}
		}
	}
	applyHooks.run(applyHooks.afterStaging)
	for _, f := range files {
		if err := f.check(root); err != nil {
			dropTemps()
			return nil, err
		}
	}
	applyHooks.run(applyHooks.beforeRename)
	for i, f := range files {
		err := f.check(root)
		if err == nil {
			if err = applyHooks.move(f.tmp, f.dest); err == nil {
				f.tmp = ""
				continue
			}
			err = fmt.Errorf("%s: %v", f.rel, err)
		}
		return nil, rollBack(root, plan, files, i, err, dropTemps)
	}
	dropTemps()
	syncDirs(files)
	return names(files), nil
}

// rollBack puts files[:done] back from their staged originals after the
// rename of files[done] failed with cause, removes every temporary file, and
// describes the outcome.
func rollBack(root string, plan *analysis.RenamePlan, files []*renameFile, done int, cause error, dropTemps func()) error {
	fail := &applyFailure{err: cause, written: []string{}}
	for j := done - 1; j >= 0; j-- {
		f := files[j]
		if err := applyHooks.move(f.back, f.dest); err != nil {
			fail.written = append([]string{f.rel}, fail.written...)
			continue
		}
		f.back = ""
		fail.rolledBack = append([]string{f.rel}, fail.rolledBack...)
	}
	dropTemps()
	still := map[string]bool{}
	for _, w := range fail.written {
		still[w] = true
	}
	for _, f := range files {
		if !still[f.rel] {
			fail.notWritten = append(fail.notWritten, f.rel)
		}
	}
	if len(fail.written) > 0 {
		fail.reverse = fmt.Sprintf("bash-lsp rename %s %s --apply --root %s (by path:line:col of each definition if the name is shared)", plan.New, plan.Old, shellQuote(root))
	}
	return fail
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// checkDirs refuses the rename when a directory of the files does not take a
// new file: the temporary files go there. The probe is a temporary file that
// is removed at once.
func checkDirs(files []*renameFile) error {
	seen := map[string]bool{}
	for _, f := range files {
		dir := filepath.Dir(f.path)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		p, err := os.CreateTemp(dir, ".bash-lsp-fmt-*")
		if err != nil {
			return &query.RenameRefusedError{Code: query.IssueReadOnly, Reason: fmt.Sprintf("the directory of %s cannot take a new file: %v", f.rel, err)}
		}
		p.Close()
		os.Remove(p.Name())
	}
	return nil
}

// syncDirs flushes the directories the files were renamed in. A failure is
// not reported: the files are in place.
func syncDirs(files []*renameFile) {
	seen := map[string]bool{}
	for _, f := range files {
		dir := filepath.Dir(f.dest)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if d, err := os.Open(dir); err == nil {
			d.Sync()
			d.Close()
		}
	}
}
