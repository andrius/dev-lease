//go:build !unix

package main

import "os"

// socketUIDGID has no native owner on non-Unix builds; the permission
// message prints the mode only there. The supported way to run dev-lease is
// the Linux container.
func socketUIDGID(fi os.FileInfo) (uid, gid uint32, ok bool) {
	return 0, 0, false
}
