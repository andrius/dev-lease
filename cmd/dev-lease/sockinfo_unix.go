//go:build unix

package main

import (
	"os"
	"syscall"
)

// socketUIDGID returns the socket file's uid and gid from its stat data.
// The permission-denied message prints them so the operator can compare the
// socket's owner against the process identity after the privilege drop.
func socketUIDGID(fi os.FileInfo) (uid, gid uint32, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return st.Uid, st.Gid, true
}
