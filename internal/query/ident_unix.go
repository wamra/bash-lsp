//go:build unix

package query

import (
	"os"
	"syscall"
)

// fileIdentity is the link count, owner and group of a file.
func fileIdentity(fi os.FileInfo) (nlink uint64, uid, gid uint32, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return uint64(st.Nlink), st.Uid, st.Gid, true
}

func preserveOwner(path string, orig os.FileInfo) error {
	_, uid, gid, ok := fileIdentity(orig)
	if !ok {
		return nil
	}
	cur, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if _, u, g, ok := fileIdentity(cur); ok && u == uid && g == gid {
		return nil
	}
	return os.Chown(path, int(uid), int(gid))
}
