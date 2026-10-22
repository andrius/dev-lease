//go:build !linux

package main

// privdrop is Linux-only; elsewhere serve runs with whatever privileges it
// has and there is nothing to drop.
func privdrop(string) error { return nil }
