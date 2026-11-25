package query

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// ApplyIssue is a reason `rename --apply` would refuse to write a file.
type ApplyIssue struct{ Code, Reason string }

// Codes of the issues that stop an apply; the first group is also reported
// by a preview as a warning, the rest depend on the files at the time of the
// apply.
const (
	IssueOutsideRoot   = "outside-root"
	IssueNotRegular    = "not-regular-file"
	IssueSpecialMode   = "special-mode"
	IssueHardLink      = "hard-link"
	IssueNotOwner      = "not-owner"
	IssueReadOnly      = "read-only"
	IssueNUL           = "nul-byte"
	IssueInvalidUTF8   = "invalid-utf8"
	IssueStale         = "stale-file"
	warnApplyWouldFail = "apply-would-refuse"
)

// effectiveUID, effectiveGID and supplementaryGroups say who writes the
// files; variables for tests.
var (
	effectiveUID        = os.Geteuid
	effectiveGID        = os.Getegid
	supplementaryGroups = os.Getgroups
)

// CheckFileMeta checks the file at path (root-relative name rel) for
// everything an apply needs except its bytes: inside the root once links are
// resolved, a regular file and not a link, no setuid, setgid or sticky bit,
// a single hard link, owned by the user writing (root excepted), writable. It
// returns the Lstat of the file.
func CheckFileMeta(root, path, rel string) (os.FileInfo, *ApplyIssue) {
	if _, ok := Inside(root, path); !ok {
		return nil, &ApplyIssue{IssueOutsideRoot, fmt.Sprintf("%s resolves outside the root", rel)}
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, &ApplyIssue{IssueStale, fmt.Sprintf("%s cannot be read any more: %v", rel, err)}
	}
	if !fi.Mode().IsRegular() {
		return fi, &ApplyIssue{IssueNotRegular, fmt.Sprintf("%s is not a regular file (a link, directory or device)", rel)}
	}
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fi, &ApplyIssue{IssueSpecialMode, fmt.Sprintf("%s has a setuid, setgid or sticky bit, which a rewrite would drop", rel)}
	}
	if nlink, uid, gid, ok := fileIdentity(fi); ok {
		if nlink > 1 {
			return fi, &ApplyIssue{IssueHardLink, fmt.Sprintf("%s has %d hard links; a rewrite would split them", rel, nlink)}
		}
		if me := effectiveUID(); me != 0 && int(uid) != me {
			return fi, &ApplyIssue{IssueNotOwner, fmt.Sprintf("%s is owned by another user; a rewrite would change its owner", rel)}
		}
		if me := effectiveUID(); me != 0 && !canSetGroup(gid, path) {
			return fi, &ApplyIssue{IssueNotOwner, fmt.Sprintf("%s belongs to a group this user is not in; a rewrite would change its group", rel)}
		}
	}
	if err := writableFile(path, fi); err != nil {
		return fi, &ApplyIssue{IssueReadOnly, fmt.Sprintf("%s cannot be written: %v", rel, err)}
	}
	return fi, nil
}

// CheckFileBytes checks the text of the file: no NUL byte, valid UTF-8, and
// (when want is not nil) equal to want, the text the plan was built from.
func CheckFileBytes(rel string, b, want []byte) *ApplyIssue {
	switch {
	case bytes.IndexByte(b, 0) >= 0:
		return &ApplyIssue{IssueNUL, fmt.Sprintf("%s holds a NUL byte", rel)}
	case !utf8.Valid(b):
		return &ApplyIssue{IssueInvalidUTF8, fmt.Sprintf("%s is not valid UTF-8", rel)}
	case want != nil && !bytes.Equal(b, want):
		return &ApplyIssue{IssueStale, fmt.Sprintf("%s changed since the plan was built", rel)}
	}
	return nil
}

// PreserveOwner gives the file at path the owner and group of orig when they
// differ, on systems that have them; an error means the ownership could not
// be kept.
func PreserveOwner(path string, orig os.FileInfo) error { return preserveOwner(path, orig) }

// canSetGroup reports whether a rewrite of the file at path can give the new
// file the group gid: it is the user's own group, one of the supplementary
// groups, or the group of the directory (where new files take the group of
// their directory, as on macOS and in setgid directories).
func canSetGroup(gid uint32, path string) bool {
	if int(gid) == effectiveGID() {
		return true
	}
	if di, err := os.Lstat(filepath.Dir(path)); err == nil {
		if _, _, dgid, ok := fileIdentity(di); ok && dgid == gid {
			return true
		}
	}
	gs, err := supplementaryGroups()
	if err != nil {
		return false
	}
	for _, g := range gs {
		if int(gid) == g {
			return true
		}
	}
	return false
}
