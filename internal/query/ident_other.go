//go:build !unix

package query

import "os"

// Link counts and owners are not read on this system: an apply does not check
// them.
func fileIdentity(fi os.FileInfo) (nlink uint64, uid, gid uint32, ok bool) { return 0, 0, 0, false }

func preserveOwner(path string, orig os.FileInfo) error { return nil }
