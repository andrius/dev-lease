//go:build unix

package lease

import (
	"os"
	"syscall"
)

// lock takes an exclusive flock on the lock file. The returned function
// releases it.
func (s Store) lock() (func(), error) {
	lf, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		lf.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) // best effort
		lf.Close()
	}, nil
}
