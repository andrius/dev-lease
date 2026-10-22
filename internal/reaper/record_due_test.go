package reaper

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

// dueStoppedWithNewZeroLabel: a stack stopped 169h ago with serve's 168h
// recorded; its (exited) containers now carry dev-lease.remove-after "0"
// (compose up --no-start, or up -d with a service that exits at once),
// and no pass has seen the label yet.
func dueStoppedWithNewZeroLabel(t *testing.T) *harness {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{docker.ProjectLabel: "myapp"})
	h.srv.StopContainer("c1")
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	d := 168 * time.Hour
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped,
			StoppedAt: testNow.Add(-169 * time.Hour), RemoveAfter: &d, RemoveAfterFrom: "serve"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.srv.SetContainerLabels("c1", map[string]string{docker.RemoveAfterLabel: "0"})
	return h
}

func TestDueStoppedNewZeroLabelRemoveNowRefuses(t *testing.T) {
	h := dueStoppedWithNewZeroLabel(t)
	_, err := h.runner().RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"})
	if !errors.Is(err, ErrNeverRemove) {
		t.Fatalf("RemoveNow err = %v, want ErrNeverRemove", err)
	}
}

func TestDueStoppedNewZeroLabelSweepKeepsVolume(t *testing.T) {
	h := dueStoppedWithNewZeroLabel(t)
	rep := h.pass(t)
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatalf("the sweep deleted the volume of a stack whose containers carry dev-lease.remove-after=0 (report %+v)", rep)
	}
}

// The same state before the deadline is protected too.
func TestNotYetDueStoppedNewZeroLabelSweepKeepsVolume(t *testing.T) {
	h := dueStoppedWithNewZeroLabel(t)
	h.cfg.Now = func() time.Time { return testNow.Add(-2 * time.Hour) }
	h.pass(t)
	h.cfg.Now = func() time.Time { return testNow.Add(30 * 24 * time.Hour) }
	h.pass(t)
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("not-due control: volume deleted")
	}
}

// A longer label on the containers of a stopped stack outlives a later,
// shorter serve setting.
func TestStoppedLongerLabelOutlivesShorterServe(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{docker.ProjectLabel: "myapp"})
	h.srv.StopContainer("c1")
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	d := 24 * time.Hour
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped,
			StoppedAt: testNow, RemoveAfter: &d, RemoveAfterFrom: "serve"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.srv.SetContainerLabels("c1", map[string]string{docker.RemoveAfterLabel: "100h"})
	h.cfg.RemoveAfter = 168 * time.Hour
	h.cfg.Now = func() time.Time { return testNow.Add(time.Hour) }
	h.pass(t)
	h.cfg.RemoveAfter = 24 * time.Hour
	h.cfg.Now = func() time.Time { return testNow.Add(25 * time.Hour) }
	h.pass(t)
	if !h.srv.Has("volume", "myapp_data") {
		t.Errorf("removed at 25h while the containers carry dev-lease.remove-after=100h")
	}
}

// A recorded label 0 counts only while no container is left: containers
// relabelled to a delay speak for themselves, as the views show them.
func TestRemoveNowFollowsRelabelledContainersOverRecord(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "720h"})
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	zero := time.Duration(0)
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", LabelRemoveAfter: &zero}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.runner().RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "dashboard"}); err != nil {
		t.Fatalf("RemoveNow err = %v, want the removal the containers' 720h label allows", err)
	}
	if h.srv.Has("volume", "myapp_data") {
		t.Fatal("the volume is still there after the removal")
	}
}
