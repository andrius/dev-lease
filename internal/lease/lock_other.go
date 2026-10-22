//go:build !unix

package lease

// lock on platforms without flock degrades to no mutual exclusion. The
// supported way to run dev-lease is the Linux container; a native Windows
// build may race if several writers update the same state file.
func (s Store) lock() (func(), error) {
	return func() {}, nil
}
