//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// privdrop drops the root privileges serve starts with: the group of the
// Docker socket is kept (that group owns the socket) and the uid becomes
// 65532 (the common nonroot uid). It is a no-op when serve does not run as root.
// It must run before any Docker call.
func privdrop(socketPath string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	fi, err := os.Stat(socketPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", socketPath, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot read the group of %s", socketPath)
	}
	gid := int(st.Gid)
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("setgroups(%d): %w", gid, err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid(%d): %w", gid, err)
	}
	if err := syscall.Setuid(65532); err != nil {
		return fmt.Errorf("setuid(65532): %w", err)
	}
	return nil
}
