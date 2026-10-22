//go:build unix

package main

import (
	"os"
	"syscall"
)

// lockServeFile takes an exclusive, non-blocking flock on the serve lock
// file: only one serve may run per state file. The kernel releases the
// lock when the process exits, so a serve that dies cannot leave it held;
// the returned function releases it explicitly.
func lockServeFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // best effort
		f.Close()
	}, nil
}
