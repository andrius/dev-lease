package reaper

import (
	"time"

	"github.com/andrius/dev-lease/internal/docker"
)

// RemoveAfterFor resolves one project's effective removal delay from the
// dev-lease.remove-after labels of its containers: no label means the
// serve setting (reported as not from a label), a 0 label wins (never
// remove), otherwise the longest value. It is the same rule the reaper
// applies when a project stops, exported so the view's live and new
// stacks show the delay that will actually apply.
func RemoveAfterFor(conts []docker.Object, serve time.Duration) (time.Duration, bool) {
	return removeAfterFor(conts, serve)
}
