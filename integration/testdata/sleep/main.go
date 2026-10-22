// Package main is a tiny static binary the integration test puts into a
// FROM scratch image, so the test project has a real container that keeps
// running and a project-built image the reaper must remove.
package main

import "time"

func main() {
	for {
		time.Sleep(time.Hour)
	}
}
