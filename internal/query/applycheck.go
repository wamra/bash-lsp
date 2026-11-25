package query

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
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

// effectiveUID is the user the files are written as; a variable for tests.
var effectiveUID = os.Geteuid

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
	if nlink, uid, _, ok := fileIdentity(fi); ok {
		if nlink > 1 {
			return fi, &ApplyIssue{IssueHardLink, fmt.Sprintf("%s has %d hard links; a rewrite would split them", rel, nlink)}
		}
		if me := effectiveUID(); me != 0 && int(uid) != me {
			return fi, &ApplyIssue{IssueNotOwner, fmt.Sprintf("%s is owned by another user; a rewrite would change its owner", rel)}
		}
	}
	if err := writableFile(path); err != nil {
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

// writableFile reports whether the file at path can be opened for writing by
// this user. Nothing is written.
func writableFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

// PreserveOwner gives the file at path the owner and group of orig when they
// differ, on systems that have them; an error means the ownership could not
// be kept.
func PreserveOwner(path string, orig os.FileInfo) error { return preserveOwner(path, orig) }
