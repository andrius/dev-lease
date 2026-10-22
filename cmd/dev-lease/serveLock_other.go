//go:build !unix

package main

// lockServeFile on platforms without flock degrades to no mutual
// exclusion. The supported way to run dev-lease is the Linux container;
// a native Windows serve cannot prevent a second serve on the same state
// file.
func lockServeFile(path string) (func(), error) {
	return func() {}, nil
}
