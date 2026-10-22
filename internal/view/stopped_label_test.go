package view

import (
	"context"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
)

// A stopped stack whose containers now carry dev-lease.remove-after "0"
// is shown as never removed before the next sweep records the label, as
// RemoveNow already refuses it.
func TestViewStoppedStackWithNewZeroLabelIsNeverRemoved(t *testing.T) {
	h := newVH(t)
	h.srv.AddLabels("container", "myapp-c1", "myapp-web", map[string]string{docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "0"})
	h.srv.StopContainer("myapp-c1")
	d := 168 * time.Hour
	h.put(t, stopped("myapp", viewNow.Add(-169*time.Hour), &d, "serve"))
	stacks, err := List(context.Background(), h.cli(), h.st, h.cfg, viewNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stacks {
		if s.Name == "myapp" {
			if !s.NeverRemove {
				t.Errorf("view says %q while the containers carry dev-lease.remove-after=0 and RemoveNow refuses", s.Next)
			}
		}
	}
}
