package analysis

import (
	"fmt"
	"strings"

	"github.com/wamra/bash-lsp/tree"
)

// cmdSite is a call that names an external command: use indexes the file's
// Uses and tab the snapshot's command path table.
type cmdSite struct{ use, tab int32 }

var codePathLookup = reasonCode(ReasonPathLookup)

// plainCommandName reports whether name is a literal command word a PATH
// lookup can answer: letters, digits and _ . + - : @ , only, and not starting
// with - or +. Anything with a slash is a path (an invocation, not a lookup),
// anything with an expansion or a glob character is not a name.
func plainCommandName(name string) bool {
	if name == "" || name[0] == '-' || name[0] == '+' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == '+', c == '-', c == ':', c == '@', c == ',':
		default:
			return false
		}
	}
	return true
}

// noteCommand records use j of file as a command call when it is a plain
// command word that binds to nothing: not a builtin, not a function or alias
// of that name anywhere in the workspace (those are candidate calls, bound
// elsewhere), not a name inside a string, an alias value or a variable. The
// name is looked up once per Build.
func (s *Snapshot) noteCommand(file, j int, u *Use) {
	if u.Via != ViaNone || u.ViaAlias || !plainCommandName(u.Name) || IsBuiltin(u.Name) ||
		len(s.funcSyms[u.Name]) > 0 || len(s.aliasSyms[u.Name]) > 0 {
		return
	}
	if s.cmdNames == nil {
		s.cmdNames = map[string]int32{}
	}
	tab, ok := s.cmdNames[u.Name]
	if !ok {
		path, found := s.cmdLook(u.Name)
		if !found {
			path = ""
		}
		tab = int32(len(s.cmdPaths))
		s.cmdPaths = append(s.cmdPaths, path)
		s.cmdNames[u.Name] = tab
	}
	if s.cmds == nil {
		s.cmds = make([][]cmdSite, len(s.paths))
	}
	s.cmds[file] = append(s.cmds[file], cmdSite{int32(j), tab})
}

// appendCommandEdges appends the command edges of file: candidate/path-lookup
// for a name the profile holds, unresolved/not-found for one it does not.
func (s *Snapshot) appendCommandEdges(out []edgeRec, file int) []edgeRec {
	if s.cmds == nil {
		return out
	}
	for _, c := range s.cmds[file] {
		r := edgeRec{at: c.use, tgt: c.tab, rel: RelCalls, cmd: true}
		if s.cmdPaths[c.tab] != "" {
			r.res, r.reason = Candidate, codePathLookup
		} else {
			r.res, r.reason = Unresolved, codeNotFound
		}
		out = append(out, r)
	}
	return out
}

// IsCommand reports whether e is a command edge: a call of an external
// command, found in the PATH profile (candidate/path-lookup, ToFile its path)
// or not (unresolved/not-found). It has no target symbol. The graph queries,
// call hierarchy and extract leave these edges out; WB1102 reports the
// missing ones.
func (e Edge) IsCommand() bool {
	return e.Rel == RelCalls && e.To == nil && e.ToDecl == nil &&
		(e.Reason == ReasonPathLookup || e.Reason == ReasonNotFound && e.Res == Unresolved)
}

// --- WB1102: missing command ---

// setsPathFor reports whether the call of u carries a prefix assignment of
// PATH (`PATH=... cmd`), which changes where that command is found.
func setsPathFor(u *Use) bool {
	if u.Node == nil {
		return false
	}
	for _, ch := range u.Node.Children {
		if ch.Kind == tree.KAssign && ch.Name == "PATH" {
			return true
		}
	}
	return false
}

// pathSetInView reports whether file, a file it sources or a file that
// sources it (the files visible from it, as for an unresolved source) assigns
// PATH: `PATH=`, `PATH+=`, `export PATH=`, `declare -x PATH=`, `local PATH=`.
// The files that do are found once per snapshot.
func (s *Snapshot) pathSetInView(file int) bool {
	s.pathOnce.Do(func() {
		s.pathFiles = newBitset(len(s.paths))
		for i, ff := range s.facts {
			if ff == nil {
				continue
			}
			for _, d := range ff.Decls {
				if d.Kind == DeclVariable && d.Name == "PATH" && !d.Temp {
					s.pathFiles.set(i)
					break
				}
			}
		}
	})
	vis := s.visible[file]
	for i, w := range s.pathFiles {
		if w&vis[i] != 0 {
			return true
		}
	}
	return false
}

// commands reports each command of the file that is in no function, alias,
// builtin or the PATH profile. It stays silent where the answer is not the
// profile's alone: in a bats file (its helpers come from libraries loaded by
// the runner), when a `source` in view could not be resolved (the command may
// be defined there), and for a name the file asks about first with
// `command -v NAME`, `type NAME`, `hash NAME` or `which NAME` (an optional
// tool).
func (c *diagCtx) commands() {
	s := c.s
	if len(s.cmds) == 0 || len(s.cmds[c.file]) == 0 || s.incomplete[c.file] || c.f.Dialect == tree.Bats {
		return
	}
	var asked map[string]bool
	pathSet, pathKnown := false, false
	for _, site := range s.cmds[c.file] {
		if s.cmdPaths[site.tab] != "" {
			continue
		}
		u := c.ff.Uses[site.use]
		if setsPathFor(u) {
			continue
		}
		if !pathKnown {
			pathSet, pathKnown = s.pathSetInView(c.file), true
		}
		if pathSet {
			continue
		}
		if asked == nil {
			asked = c.askedCommands()
		}
		if asked[u.Name] {
			continue
		}
		c.add("WB1102", u.Range, fmt.Sprintf("command not found: %s is no function, alias or builtin in view and is not on the PATH profile (unresolved/%s)", u.Name, ReasonNotFound))
	}
}

// askedCommands is the set of literal words that follow `command`, `type`,
// `hash` or `which` anywhere in the file.
func (c *diagCtx) askedCommands() map[string]bool {
	out := map[string]bool{}
	for _, u := range c.ff.Uses {
		if u.Kind != UseCall || u.Node == nil {
			continue
		}
		switch u.Name {
		case "command", "type", "hash", "which":
		default:
			continue
		}
		for _, w := range u.Node.Children {
			if w.Kind != tree.KWord {
				continue
			}
			if lit, ok := literalText(w); ok && lit != "" && !strings.HasPrefix(lit, "-") {
				out[lit] = true
			}
		}
	}
	return out
}
