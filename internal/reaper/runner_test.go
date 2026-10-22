package reaper

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

// extendWith is h.extend with a named holder.
func (h *harness) extendWith(project, holder string, ttl time.Duration, at time.Time) {
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		l, err := lease.Extend(m[project], holder, ttl, at)
		if err != nil {
			return err
		}
		if l.State == lease.StateStopped {
			l.State = ""
			l.StoppedAt = time.Time{}
		}
		m[project] = l
		return nil
	}); err != nil {
		panic(err)
	}
}

// runner builds the Runner under test on the harness.
func (h *harness) runner() *Runner {
	h.cfg.Log = h.log
	return &Runner{Cli: docker.New(h.srv.Socket), Store: h.st, Cfg: h.cfg}
}

func TestStopNowEndsClaimsAndStops(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extendWith("myapp", "ci", time.Hour, testNow)
	h.extendWith("myapp", "bob", 30*time.Minute, testNow)

	r := h.runner()
	rep, err := r.StopNow(context.Background(), "myapp", "dashboard")
	if err != nil {
		t.Fatalf("StopNow: %v", err)
	}
	if len(rep.Failed) != 0 || len(rep.Removed) != 0 {
		t.Fatalf("report = %+v, want no failures and no removals", rep)
	}
	l := h.leases()["myapp"]
	if l.State != lease.StateStopped {
		t.Fatalf("lease state = %q, want stopped", l.State)
	}
	for _, holder := range l.Holders {
		if holder.Expires.After(testNow) {
			t.Errorf("holder %s expires at %v, want every claim ended at %v", holder.Name, holder.Expires, testNow)
		}
	}
	if !h.srv.IsStopped("container", "c1") || !h.srv.IsStopped("container", "c2") {
		t.Fatal("StopNow did not stop the containers")
	}
	for _, kind := range []string{"container", "network", "volume", "image"} {
		if h.srv.Count(kind) == 0 {
			t.Errorf("StopNow removed %s objects, want stop only", kind)
		}
	}
}

func TestRemoveNowFromLiveStopsThenRemoves(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Hour, testNow)

	r := h.runner()
	rep, err := r.RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"})
	if err != nil {
		t.Fatalf("RemoveNow: %v", err)
	}
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	for _, kind := range []string{"container", "network", "volume", "image"} {
		if got := h.srv.Count(kind); got != 0 {
			t.Errorf("fake daemon still has %d %s objects, want 0", got, kind)
		}
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared after removal")
	}
	// Stops must precede deletes on every object.
	lastStop, firstDelete := -1, -1
	for i, call := range h.srv.Calls() {
		switch {
		case call.Method == "POST" && strings.HasSuffix(call.Path, "/stop"):
			lastStop = i
		case call.Method == "DELETE":
			if firstDelete == -1 {
				firstDelete = i
			}
		}
	}
	if lastStop == -1 || firstDelete == -1 {
		t.Fatalf("stops or deletes missing from the recorded calls: %+v", h.srv.Calls())
	}
	if firstDelete < lastStop {
		t.Fatalf("a delete at %d ran before the last stop at %d", firstDelete, lastStop)
	}
}

func TestRemoveNowKeepsKeptObjects(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	srv.AddLabels("image", "sha256:i1", "myapp-web:latest", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.KeepLabel: "true",
	}, "sha256:i1", []string{"myapp_default"}, []string{"myapp_data"})
	srv.AddContainerWithImage("c2", "db", "myapp", "sha256:i1")
	srv.Add("network", "n1", "myapp_default", "myapp")
	srv.Add("volume", "v1", "myapp_data", "myapp")
	h.extend("myapp", time.Hour, testNow)

	r := h.runner()
	rep, err := r.RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"})
	if err != nil {
		t.Fatalf("RemoveNow: %v", err)
	}
	if len(rep.Failed) != 0 || len(rep.Removed) != 1 {
		t.Fatalf("report = %+v, want removed without failures", rep)
	}
	if !srv.Has("container", "c1") || srv.IsStopped("container", "c1") {
		t.Fatal("kept container stopped or removed")
	}
	if srv.Has("container", "c2") {
		t.Fatal("db container survived the removal")
	}
	if !srv.Has("network", "n1") || !srv.Has("volume", "myapp_data") || !srv.Has("image", "sha256:i1") {
		t.Fatal("network, volume or image the kept container uses was removed")
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared after removal")
	}
}

func TestRemoveNowRefusesNeverRemoveUnlessAllowed(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	srv.Add("image", "sha256:i1", "myapp-web:latest", "myapp")
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "0",
	}, "sha256:i1", nil, nil)
	srv.AddContainerWithImage("c2", "db", "myapp", "sha256:i1")
	srv.Add("network", "n1", "myapp_default", "myapp")
	srv.Add("volume", "v1", "myapp_data", "myapp")
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t) // the expiry pass stops the stack and records RemoveAfter 0

	r := h.runner()
	if _, err := r.RemoveNow(context.Background(), "myapp", RemoveOptions{}); !errors.Is(err, ErrNeverRemove) {
		t.Fatalf("RemoveNow error = %v, want ErrNeverRemove", err)
	}
	if h.srv.Count("container") != 2 || !h.srv.IsStopped("container", "c1") {
		t.Fatalf("the refused removal touched the daemon: containers = %d", h.srv.Count("container"))
	}
	if l := h.leases()["myapp"]; l.State != lease.StateStopped {
		t.Fatalf("lease = %+v, want still stopped", l)
	}

	rep, err := r.RemoveNow(context.Background(), "myapp", RemoveOptions{AllowNeverRemove: true})
	if err != nil {
		t.Fatalf("RemoveNow with AllowNeverRemove: %v", err)
	}
	if len(rep.Removed) != 1 {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared after the allowed removal")
	}
}

func TestRunnerActionsRefusedWhileBusy(t *testing.T) {
	for _, state := range []string{lease.StateStopping, lease.StateRemoving} {
		t.Run("when "+state+", expect ErrBusy", func(t *testing.T) {
			h := newHarness(t)
			fullProject(h.srv)
			h.extend("myapp", time.Hour, testNow)
			if err := h.st.Update(func(m map[string]lease.Lease) error {
				l := m["myapp"]
				l.State = state
				m["myapp"] = l
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			r := h.runner()
			if _, err := r.StopNow(context.Background(), "myapp", "dashboard"); !errors.Is(err, ErrBusy) {
				t.Errorf("StopNow error = %v, want ErrBusy", err)
			}
			if _, err := r.RemoveNow(context.Background(), "myapp", RemoveOptions{}); !errors.Is(err, ErrBusy) {
				t.Errorf("RemoveNow error = %v, want ErrBusy", err)
			}
			// Nothing moved on the daemon.
			if h.srv.Count("container") != 2 || h.srv.Count("volume") != 1 {
				t.Fatalf("the refused actions touched the daemon: %d containers, %d volumes", h.srv.Count("container"), h.srv.Count("volume"))
			}
		})
	}
}

// neverRemoveStoppedThenDown stops a stack whose containers carry
// dev-lease.remove-after=0, then deletes the containers from the fake like
// a compose down would (the volume stays) and ages the stop.
func neverRemoveStoppedThenDown(t *testing.T) *harness {
	h := newHarness(t)
	h.srv.Add("image", "sha256:i1", "myapp-web:latest", "myapp")
	for _, id := range []string{"c1", "c2"} {
		h.srv.AddContainerWithUses(id, id, map[string]string{
			docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "0",
		}, "sha256:i1", nil, []string{"myapp_data"})
	}
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	h.pass(t)
	l := h.leases()["myapp"]
	if l.State != lease.StateStopped || l.RemoveAfter == nil || *l.RemoveAfter != 0 || l.RemoveAfterFrom != "label" {
		t.Fatalf("setup: lease = %+v, want stopped with recorded 0 from label", l)
	}
	cli := docker.New(h.srv.Socket)
	for _, id := range []string{"c1", "c2"} {
		if err := cli.RemoveContainer(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		l := m["myapp"]
		l.StoppedAt = testNow.Add(-30 * 24 * time.Hour)
		m["myapp"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Control: the sweep alone never removes a never-remove volume.
	h.pass(t)
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("control failed: the sweep removed a never-remove volume on its own")
	}
	return h
}

// A re-stop of a never-remove stack keeps the recorded 0 from the label:
// a later sweep removes nothing.
func TestRestopKeepsRecordedNeverRemove(t *testing.T) {
	h := neverRemoveStoppedThenDown(t)
	if _, err := h.runner().StopNow(context.Background(), "myapp", "dashboard"); err != nil {
		t.Fatalf("StopNow: %v", err)
	}
	l := h.leases()["myapp"]
	if l.RemoveAfter == nil || *l.RemoveAfter != 0 || l.RemoveAfterFrom != "label" {
		t.Fatalf("lease after StopNow = %+v, want the recorded 0 from label kept", l)
	}
	h.pass(t)
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("a stop of a never-remove stack made the next sweep delete its volume")
	}
}

// A re-stop never lowers a recorded serve-set delay: the longer of the
// recorded 720h and the current 168h stays.
func TestRestopKeepsLongerRecordedDelay(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	long := 720 * time.Hour
	h.srv.StopContainer("c1")
	h.srv.StopContainer("c2")
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped,
			StoppedAt: testNow.Add(-8 * 24 * time.Hour), RemoveAfter: &long, RemoveAfterFrom: "serve"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.pass(t) // control: 720h has not passed, nothing is removed
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatalf("control failed: removed before the recorded 720h: %s", h.log.String())
	}
	if _, err := h.runner().StopNow(context.Background(), "myapp", "dashboard"); err != nil {
		t.Fatal(err)
	}
	l := h.leases()["myapp"]
	if l.RemoveAfter == nil || *l.RemoveAfter != long {
		t.Fatalf("lease after StopNow = %+v, want the recorded 720h kept", l)
	}
	h.pass(t)
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("StopNow re-recorded the delay as serve's 168h: removal 22 days before the recorded 720h")
	}
}

// A re-stop that turns up a shorter label must not move the removal
// deadline earlier than the recorded serve-set delay: the effective delays
// (each capped from below by the current serve setting) are compared, not
// the raw values.
func TestRestopKeepsLongerEffectiveDelay(t *testing.T) {
	h := newHarness(t)
	h.cfg.RemoveAfter = 720 * time.Hour
	fullProject(h.srv)
	h.srv.StopContainer("c1")
	h.srv.StopContainer("c2")
	rec := 168 * time.Hour
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped,
			StoppedAt: testNow.Add(-10 * 24 * time.Hour), RemoveAfter: &rec, RemoveAfterFrom: "serve"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The containers now carry a 200h label: longer in raw terms than the
	// recorded 168h, but shorter than the serve's current 720h floor.
	for _, id := range []string{"c1", "c2"} {
		h.srv.SetContainerLabels(id, map[string]string{docker.RemoveAfterLabel: "200h"})
	}
	if _, err := h.runner().StopNow(context.Background(), "myapp", "dashboard"); err != nil {
		t.Fatal(err)
	}
	l := h.leases()["myapp"]
	if l.RemoveAfter == nil || *l.RemoveAfter != rec || l.RemoveAfterFrom != "serve" {
		t.Fatalf("lease after StopNow = %+v, want the recorded 168h kept (effective 720h)", l)
	}
	h.pass(t)
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("the shorter label moved the deadline back to day 8: the sweep removed the stack")
	}
}

// When the re-read label and the recorded value give the same effective
// delay, the recorded origin is kept: a stop must not churn the origin.
func TestRestopTieKeepsRecordedOrigin(t *testing.T) {
	h := newHarness(t)
	h.cfg.RemoveAfter = 720 * time.Hour
	fullProject(h.srv)
	h.srv.StopContainer("c1")
	h.srv.StopContainer("c2")
	rec := 168 * time.Hour
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped,
			StoppedAt: testNow.Add(-time.Hour), RemoveAfter: &rec, RemoveAfterFrom: "serve"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// 720h from the labels equals max(recorded 168h, serve 720h): a tie at
	// the effective level.
	for _, id := range []string{"c1", "c2"} {
		h.srv.SetContainerLabels(id, map[string]string{docker.RemoveAfterLabel: "720h"})
	}
	if _, err := h.runner().StopNow(context.Background(), "myapp", "dashboard"); err != nil {
		t.Fatal(err)
	}
	l := h.leases()["myapp"]
	if l.RemoveAfter == nil || *l.RemoveAfter != rec || l.RemoveAfterFrom != "serve" {
		t.Fatalf("lease after StopNow = %+v, want the recorded 168h from serve kept on the tie", l)
	}
}

// A recorded stop without a recorded delay counts as serve's current
// setting in the re-stop merge: a shorter re-read label must not move the
// deadline earlier than that setting.
func TestRestopNoRecordedDelayCountsAsServe(t *testing.T) {
	h := newHarness(t)
	h.cfg.RemoveAfter = 720 * time.Hour
	fullProject(h.srv)
	h.srv.StopContainer("c1")
	h.srv.StopContainer("c2")
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped,
			StoppedAt: testNow.Add(-10 * 24 * time.Hour)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"c1", "c2"} {
		h.srv.SetContainerLabels(id, map[string]string{docker.RemoveAfterLabel: "200h"})
	}
	if _, err := h.runner().StopNow(context.Background(), "myapp", "dashboard"); err != nil {
		t.Fatal(err)
	}
	l := h.leases()["myapp"]
	if l.RemoveAfter == nil || *l.RemoveAfter != 720*time.Hour || l.RemoveAfterFrom != "serve" {
		t.Fatalf("lease after StopNow = %+v, want 720h from serve", l)
	}
	h.pass(t)
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("the 200h label overrode the serve delay: the sweep removed the stack")
	}
}

// A new never-remove label on a re-stop wins over the recorded serve-set
// delay.
func TestRestopTakesNewZeroLabel(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.StopContainer("c1")
	h.srv.StopContainer("c2")
	after := 168 * time.Hour
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped,
			StoppedAt: testNow.Add(-time.Hour), RemoveAfter: &after, RemoveAfterFrom: "serve"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The containers now carry the never-remove label.
	for _, id := range []string{"c1", "c2"} {
		h.srv.SetContainerLabels(id, map[string]string{docker.RemoveAfterLabel: "0"})
	}
	if _, err := h.runner().StopNow(context.Background(), "myapp", "dashboard"); err != nil {
		t.Fatal(err)
	}
	l := h.leases()["myapp"]
	if l.RemoveAfter == nil || *l.RemoveAfter != 0 || l.RemoveAfterFrom != "label" {
		t.Fatalf("lease after StopNow = %+v, want 0 from label", l)
	}
}

// A re-stop while serve runs with --remove-after 0 keeps the recorded
// delay and a nil recorded delay stays nil: the off switch must not be
// written into the lease, so when the 0 is lifted the delays apply again
// under the new setting and stacks already past theirs go at the next
// sweep.
func TestRestopServeZeroKeepsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  *time.Duration
	}{
		{"recorded 168h from serve", func() *time.Duration { d := 168 * time.Hour; return &d }()},
		{"no recorded delay", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			fullProject(h.srv)
			h.srv.StopContainer("c1")
			h.srv.StopContainer("c2")
			if err := h.st.Update(func(m map[string]lease.Lease) error {
				l := lease.Lease{Project: "myapp", State: lease.StateStopped, StoppedAt: testNow.Add(-10 * 24 * time.Hour), RemoveAfter: tc.rec}
				if tc.rec != nil {
					l.RemoveAfterFrom = "serve"
				}
				m["myapp"] = l
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			h.cfg.RemoveAfter = 0 // serve restarted with --remove-after 0
			if _, err := h.runner().StopNow(context.Background(), "myapp", "mcp"); err != nil {
				t.Fatal(err)
			}
			h.cfg.RemoveAfter = 168 * time.Hour // the 0 is lifted
			h.pass(t)
			if h.srv.Has("volume", "myapp_data") {
				t.Fatal("the 0 was lifted and the stack is 10 days past its 168h, but the sweep kept it: the re-stop froze it as never remove")
			}
		})
	}
}

// A recorded label delay is its own effective value, never raised by
// serve's setting. Recorded label 100h, serve 168h, stopped 120h ago;
// the labels now say 150h. The longer label wins, so the sweep must not
// remove at 120h.
func TestRestopLongerLabelWinsOverRecordedLabel(t *testing.T) {
	h := newHarness(t)
	h.cfg.RemoveAfter = 168 * time.Hour
	fullProject(h.srv)
	h.srv.StopContainer("c1")
	h.srv.StopContainer("c2")
	rec := 100 * time.Hour
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped,
			StoppedAt: testNow.Add(-120 * time.Hour), RemoveAfter: &rec, RemoveAfterFrom: "label"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"c1", "c2"} {
		h.srv.SetContainerLabels(id, map[string]string{docker.RemoveAfterLabel: "150h"})
	}
	if _, err := h.runner().StopNow(context.Background(), "myapp", "mcp"); err != nil {
		t.Fatal(err)
	}
	h.pass(t)
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatalf("removed at 120h though the re-read label says 150h: %+v", h.leases()["myapp"])
	}
}

// A StopNow on a stopped lease keeps the recorded StoppedAt: the removal
// deadline must not restart.
func TestRestopKeepsStoppedAt(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.StopContainer("c1")
	h.srv.StopContainer("c2")
	at := testNow.Add(-3 * 24 * time.Hour)
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped, StoppedAt: at}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.runner().StopNow(context.Background(), "myapp", "dashboard"); err != nil {
		t.Fatal(err)
	}
	if l := h.leases()["myapp"]; !l.StoppedAt.Equal(at) {
		t.Fatalf("StoppedAt = %v, want the recorded %v", l.StoppedAt, at)
	}
}

// A stack on the daemon without a lease yet ("new" in the view) is
// stopped and recorded like any other.
func TestStopNowStopsUntrackedStack(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	rep, err := h.runner().StopNow(context.Background(), "myapp", "dashboard")
	if err != nil {
		t.Fatalf("StopNow: %v", err)
	}
	if len(rep.Failed) != 0 || len(rep.Removed) != 0 {
		t.Fatalf("report = %+v, want no failures and no removals", rep)
	}
	if l := h.leases()["myapp"]; l.State != lease.StateStopped {
		t.Fatalf("lease = %+v, want stopped", l)
	}
	for _, id := range []string{"c1", "c2"} {
		if !h.srv.IsStopped("container", id) {
			t.Fatalf("StopNow did not stop %s", id)
		}
	}
	for _, kind := range []string{"container", "network", "volume", "image"} {
		if h.srv.Count(kind) == 0 {
			t.Errorf("StopNow removed %s objects, want stop only", kind)
		}
	}
}

// A stack on the daemon without a lease yet is removed in one call.
func TestRemoveNowRemovesUntrackedStack(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	rep, err := h.runner().RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"})
	if err != nil {
		t.Fatalf("RemoveNow: %v", err)
	}
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	for _, kind := range []string{"container", "network", "volume", "image"} {
		if got := h.srv.Count(kind); got != 0 {
			t.Errorf("fake daemon still has %d %s objects, want 0", got, kind)
		}
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared after the removal")
	}
}

// A name without a lease and without anything on the daemon: ErrNoStack,
// and no lease entry appears.
func TestStopNowNoLeaseNoStackIsErrNoStack(t *testing.T) {
	h := newHarness(t)
	if _, err := h.runner().StopNow(context.Background(), "myapp", "dashboard"); !errors.Is(err, ErrNoStack) {
		t.Fatalf("StopNow error = %v, want ErrNoStack", err)
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("StopNow created a lease for an empty project")
	}
}

// A live never-remove stack: the removal is refused before anything
// changes - no claim released, no state change, no daemon POST or DELETE.
func TestRemoveNowNeverRemoveChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "0",
	}, "sha256:i1", nil, nil)
	h.extendWith("myapp", "alice", 4*time.Hour, testNow)
	raw, err := os.ReadFile(h.st.Path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.runner().RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"})
	if !errors.Is(err, ErrNeverRemove) {
		t.Fatalf("RemoveNow error = %v, want ErrNeverRemove", err)
	}
	afterRaw, err := os.ReadFile(h.st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, afterRaw) {
		t.Fatal("the refused removal changed the state file")
	}
	for _, call := range h.srv.Calls() {
		if call.Method == "POST" || call.Method == "DELETE" {
			t.Fatalf("the refused removal touched the daemon: %s %s", call.Method, call.Path)
		}
	}
	if h.srv.IsStopped("container", "c1") {
		t.Fatal("the refused removal stopped the container")
	}
	l := h.leases()["myapp"]
	if l.State != "" || len(l.Holders) != 1 || !l.Holders[0].Expires.After(testNow) {
		t.Fatalf("lease = %+v, want alice's live claim untouched", l)
	}
}

// A stopped never-remove project whose containers are already gone: the
// recorded 0 still refuses the removal.
func TestRemoveNowHonoursRecordedZero(t *testing.T) {
	h := newHarness(t)
	zero := time.Duration(0)
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped,
			StoppedAt: testNow.Add(-time.Hour), RemoveAfter: &zero, RemoveAfterFrom: "label"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := h.runner().RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"})
	if !errors.Is(err, ErrNeverRemove) {
		t.Fatalf("RemoveNow error = %v, want ErrNeverRemove", err)
	}
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("the recorded never-remove volume was deleted")
	}
}

// The never-remove pre-check runs before anything changes. Here the serve
// itself sets remove-after to 0: RemoveNow must refuse up front, leaving
// the claims, the state file and the running stack untouched.
func TestRemoveNowServeZeroRefusesBeforeStop(t *testing.T) {
	h := newHarness(t)
	h.cfg.RemoveAfter = 0
	h.srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "720h",
	}, "sha256:i1", nil, nil)
	h.extendWith("myapp", "alice", 4*time.Hour, testNow)
	raw, err := os.ReadFile(h.st.Path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.runner().RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"})
	if !errors.Is(err, ErrNeverRemove) {
		t.Fatalf("RemoveNow error = %v, want ErrNeverRemove", err)
	}
	afterRaw, err := os.ReadFile(h.st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, afterRaw) {
		t.Fatal("the refused removal changed the state file")
	}
	for _, call := range h.srv.Calls() {
		if call.Method == "POST" || call.Method == "DELETE" {
			t.Fatalf("the refused removal touched the daemon: %s %s", call.Method, call.Path)
		}
	}
	if h.srv.IsStopped("container", "c1") {
		t.Fatal("the refused removal stopped the container")
	}
	l := h.leases()["myapp"]
	if l.State != "" || len(l.Holders) != 1 || !l.Holders[0].Expires.After(testNow) {
		t.Fatalf("lease = %+v, want alice's live claim untouched", l)
	}
}

// The same under serve 0 for a stack without a lease yet: no lease means no
// recorded or effective delay to read, so only serve's own setting can
// refuse before the stop.
func TestRemoveNowServeZeroRefusesUntrackedBeforeStop(t *testing.T) {
	h := newHarness(t)
	h.cfg.RemoveAfter = 0
	h.srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "720h",
	}, "sha256:i1", nil, nil)
	_, err := h.runner().RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"})
	if !errors.Is(err, ErrNeverRemove) {
		t.Fatalf("RemoveNow error = %v, want ErrNeverRemove", err)
	}
	for _, call := range h.srv.Calls() {
		if call.Method == "POST" || call.Method == "DELETE" {
			t.Fatalf("the refused removal touched the daemon: %s %s", call.Method, call.Path)
		}
	}
	if h.srv.IsStopped("container", "c1") {
		t.Fatal("the refused removal stopped the container")
	}
	if l, ok := h.leases()["myapp"]; ok {
		t.Fatalf("the refused removal created a lease: %+v", l)
	}
}

// The refusal is reported in the action log, so the caller's dashboard can
// show it: one remove line naming the project and the MCP via.
func TestRemoveNowRefusalWritesActionLine(t *testing.T) {
	h := newHarness(t)
	h.cfg.RemoveAfter = 0
	h.srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp",
	}, "sha256:i1", nil, nil)
	h.extendWith("myapp", "alice", 4*time.Hour, testNow)
	_, err := h.runner().RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"})
	if !errors.Is(err, ErrNeverRemove) {
		t.Fatalf("RemoveNow error = %v, want ErrNeverRemove", err)
	}
	found := false
	for _, line := range h.logLines() {
		result, _ := line["result"].(string)
		if line["action"] == "remove" && line["project"] == "myapp" && line["via"] == "mcp" &&
			strings.Contains(result, "never remove") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no remove action line records the refusal: %s", h.log.String())
	}
}

// A stopped project whose recorded remove-after is 0 stays refused even
// when its containers are running again and now carry a 720h label: the
// recorded 0 outranks the re-read labels.
func TestRemoveNowRecordedZeroRefused(t *testing.T) {
	h := newHarness(t)
	zero := time.Duration(0)
	h.srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "720h",
	}, "sha256:i1", nil, nil)
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped,
			StoppedAt: testNow.Add(-time.Hour), RemoveAfter: &zero, RemoveAfterFrom: "label"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(h.st.Path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.runner().RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"})
	if !errors.Is(err, ErrNeverRemove) {
		t.Fatalf("RemoveNow error = %v, want ErrNeverRemove", err)
	}
	afterRaw, err := os.ReadFile(h.st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, afterRaw) {
		t.Fatal("the refused removal changed the state file")
	}
	if h.srv.IsStopped("container", "c1") {
		t.Fatal("the refused removal stopped the restarted container")
	}
	for _, call := range h.srv.Calls() {
		if call.Method == "POST" || call.Method == "DELETE" {
			t.Fatalf("the refused removal touched the daemon: %s %s", call.Method, call.Path)
		}
	}
}

// A lease with no recorded 0 whose labels flip to 0 between the pre-check
// and the stop must still refuse the removal: the post-stop check keeps the
// guarantee alive after the stack has been stopped.
func TestRemoveNowPostStopCheckRefuses(t *testing.T) {
	h := newHarness(t)
	h.srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp",
	}, "sha256:i1", nil, []string{"myapp_data"})
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extendWith("myapp", "alice", 4*time.Hour, testNow)
	var listings atomic.Int32
	h.srv.OnRequest(func(method, path string) {
		if method == "GET" && strings.Contains(path, "/containers/json") && listings.Add(1) == 2 {
			// The second container listing is the stop's: flip the label
			// after the pre-check has passed it.
			h.srv.SetContainerLabels("c1", map[string]string{docker.RemoveAfterLabel: "0"})
		}
	})
	_, err := h.runner().RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"})
	if !errors.Is(err, ErrNeverRemove) {
		t.Fatalf("RemoveNow error = %v, want ErrNeverRemove", err)
	}
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("a stop that turned up a 0 label still deleted the volume")
	}
	l := h.leases()["myapp"]
	if l.State != lease.StateStopped || l.RemoveAfter == nil || *l.RemoveAfter != 0 {
		t.Fatalf("lease = %+v, want stopped with the 0 recorded", l)
	}
}

// RemoveNow of a project that exists nowhere is ErrNoStack even when the
// serve sets remove-after 0: the stack existence check runs first.
func TestRemoveNowNothingAnywhereIsErrNoStack(t *testing.T) {
	h := newHarness(t)
	h.cfg.RemoveAfter = 0
	_, err := h.runner().RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"})
	if !errors.Is(err, ErrNoStack) {
		t.Fatalf("RemoveNow error = %v, want ErrNoStack", err)
	}
}

// A StopNow queued behind a pass and cancelled meanwhile returns ctx.Err()
// and touches nothing: no state change, no daemon POST.
func TestQueuedActionAfterCancelDoesNothing(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.Add("container", "o1", "other-web", "other")
	h.extend("myapp", time.Hour, testNow)
	h.extend("other", time.Minute, testNow.Add(-2*time.Minute))
	r := h.runner()

	inPass, releasePass := make(chan struct{}), make(chan struct{})
	h.srv.BeforeRemove("container", "o1", func() { close(inPass); <-releasePass })
	passDone := make(chan struct{})
	go func() { defer close(passDone); r.Pass(context.Background()) }()
	<-inPass

	ctx, cancel := context.WithCancel(context.Background())
	actionDone := make(chan error, 1)
	go func() {
		_, err := r.StopNow(ctx, "myapp", "dashboard")
		actionDone <- err
	}()
	time.Sleep(200 * time.Millisecond) // the action is queued on the runner
	cancel()
	close(releasePass)
	<-passDone
	if err := <-actionDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("StopNow error = %v, want context.Canceled", err)
	}
	for _, id := range []string{"c1", "c2"} {
		if h.srv.IsStopped("container", id) {
			t.Fatalf("the cancelled StopNow stopped %s", id)
		}
	}
	l := h.leases()["myapp"]
	if l.State != "" || len(l.Holders) != 1 {
		t.Fatalf("lease = %+v, want the untouched live lease", l)
	}
}

// The actions use the same config defaults PassWith applies: a zero
// StopTimeout becomes 10 s on the daemon call.
func TestRunnerAppliesConfigDefaults(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Hour, testNow)
	h.cfg.StopTimeout = 0
	if _, err := h.runner().StopNow(context.Background(), "myapp", "dashboard"); err != nil {
		t.Fatal(err)
	}
	var asked bool
	for _, call := range h.srv.Calls() {
		if call.Method == "POST" && strings.Contains(call.Path, "/c1/") && strings.HasSuffix(call.Path, "/stop") {
			asked = true
			if got := call.Query.Get("t"); got != "10" {
				t.Fatalf("stop timeout = %q, want the default 10s", got)
			}
		}
	}
	if !asked {
		t.Fatal("no stop call reached the daemon")
	}
}

func TestRemoveNowOfVanishedStack(t *testing.T) {
	h := newHarness(t)
	r := h.runner()
	_, err := r.RemoveNow(context.Background(), "myapp", RemoveOptions{})
	if !errors.Is(err, ErrNoStack) {
		t.Fatalf("RemoveNow error = %v, want ErrNoStack", err)
	}
	for _, call := range h.srv.Calls() {
		if call.Method == "DELETE" {
			t.Fatalf("a vanished stack still reached the daemon: %s %s", call.Method, call.Path)
		}
	}
}

func TestConcurrentRemoveNowRunsOnce(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Hour, testNow)
	r := h.runner()

	// The first removal is blocked inside a daemon call; the second
	// waits on the runner. The version handshake is left alone: it is
	// cached by the client, so blocking there would hold the second
	// caller on the cache instead of the runner. Only the first caller
	// blocks (an atomic, not sync.Once: a blocked Once would hold every
	// later caller in the hook and hide a missing runner mutex). The
	// release runs on every exit path: a Fatal while the hook is
	// blocked would otherwise deadlock the fake server's cleanup.
	inFirst, releaseFirst := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseFirst) })
	var blockedFirst atomic.Bool
	h.srv.OnRequest(func(method, path string) {
		if strings.Contains(path, "/version") {
			return
		}
		if blockedFirst.CompareAndSwap(false, true) {
			close(inFirst)
			<-releaseFirst
		}
	})

	done := []chan error{make(chan error, 1), make(chan error, 1)}
	var wg sync.WaitGroup
	wg.Add(len(done))
	for i := range done {
		go func(i int) {
			defer wg.Done()
			_, err := r.RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "dashboard"})
			done[i] <- err
		}(i)
	}
	// Whatever the outcome, wait for both removals before the fake
	// server's cleanup runs: a Fatal while one is blocked must not leave
	// the cleanup racing it.
	defer func() {
		release()
		wg.Wait()
	}()
	<-inFirst

	// While the first holds the runner, the second has neither returned
	// nor completed a single daemon call.
	select {
	case err := <-done[0]:
		t.Fatalf("a RemoveNow returned while the other held the runner: %v", err)
	case err := <-done[1]:
		t.Fatalf("a RemoveNow returned while the other held the runner: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	for _, call := range h.srv.Calls() {
		if strings.Contains(call.Path, "/version") {
			continue
		}
		t.Fatalf("the daemon completed %s %s while the first removal was blocked", call.Method, call.Path)
	}
	release()

	errs := []error{<-done[0], <-done[1]}
	var removed, vanished int
	for _, err := range errs {
		switch {
		case err == nil:
			removed++
		case errors.Is(err, ErrNoStack), errors.Is(err, ErrBusy):
			vanished++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if removed != 1 || vanished != 1 {
		t.Fatalf("removed %d, vanished %d, want one of each", removed, vanished)
	}
	// One removal only: 2 containers + 1 network + 1 volume + 1 image.
	var deletes int
	for _, call := range h.srv.Calls() {
		if call.Method == "DELETE" {
			deletes++
		}
	}
	if deletes != 5 {
		t.Fatalf("deletes = %d, want 5 for one removal", deletes)
	}
}

func TestRunnerSerialisesPassAndAction(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.srv.Add("container", "o1", "other-web", "other")
	h.extend("myapp", time.Hour, testNow)
	h.extend("other", time.Minute, testNow.Add(-2*time.Minute))
	r := h.runner()

	inPass, releasePass := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(releasePass) })
	h.srv.BeforeRemove("container", "o1", func() { close(inPass); <-releasePass })
	passDone := make(chan struct{})
	go func() {
		defer close(passDone)
		if _, err := r.Pass(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	<-inPass // the pass holds the runner, blocked in a daemon call

	actionDone := make(chan struct{})
	go func() {
		defer close(actionDone)
		if _, err := r.StopNow(context.Background(), "myapp", "dashboard"); err != nil {
			t.Error(err)
		}
	}()
	// Whatever the outcome, both goroutines finish before the fake
	// server's cleanup runs: a Fatal while the runner is blocked must not
	// leave the cleanup racing them.
	defer func() {
		release()
		<-passDone
		<-actionDone
	}()

	// The queued action waits for the pass: it has neither returned nor
	// touched the daemon while the pass is blocked.
	select {
	case <-actionDone:
		t.Fatal("the action returned while the pass held the runner")
	case <-time.After(200 * time.Millisecond):
	}
	for _, call := range h.srv.Calls() {
		if strings.Contains(call.Path, "/c1") || strings.Contains(call.Path, "/c2") {
			t.Fatalf("the waiting action touched the daemon: %s %s", call.Method, call.Path)
		}
	}

	release()
	<-passDone
	<-actionDone

	// The pass touched only other (o1), the action only myapp (c1, c2),
	// and every pass call ran before the action's.
	lastOther, firstMyapp := -1, -1
	for i, call := range h.srv.Calls() {
		switch {
		case strings.Contains(call.Path, "/o1"):
			lastOther = i
		case strings.Contains(call.Path, "/c1"), strings.Contains(call.Path, "/c2"):
			if firstMyapp == -1 {
				firstMyapp = i
			}
		}
	}
	if firstMyapp == -1 || lastOther == -1 {
		t.Fatalf("myapp or other untouched: %+v", h.srv.Calls())
	}
	if firstMyapp < lastOther {
		t.Fatalf("the action's calls interleaved with the pass's: first myapp call at %d, last other call at %d", firstMyapp, lastOther)
	}
}

// A pass queued behind an action waits for it instead of returning at
// once.
func TestPassWaitsForAction(t *testing.T) {
	h := newHarness(t)
	fullProject(h.srv)
	h.extend("myapp", time.Hour, testNow)
	r := h.runner()

	inAction, releaseAction := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseAction) })
	defer release() // a Fatal while the action is blocked must not deadlock the fake's cleanup
	h.srv.BeforeRemove("container", "c1", func() { close(inAction); <-releaseAction })
	actionDone := make(chan struct{})
	go func() {
		defer close(actionDone)
		if _, err := r.StopNow(context.Background(), "myapp", "dashboard"); err != nil {
			t.Error(err)
		}
	}()
	<-inAction // the action holds the runner, blocked in a daemon call

	passDone := make(chan error, 1)
	go func() {
		_, err := r.Pass(context.Background())
		passDone <- err
	}()
	select {
	case err := <-passDone:
		t.Fatalf("the pass returned while the action held the runner: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	release()
	<-actionDone
	if err := <-passDone; err != nil {
		t.Fatal(err)
	}
}

// The untracked stop must not hold the state file lock across its daemon
// calls: a Store.Read from another goroutine proceeds while the daemon
// listing is pending.
func TestUntrackedStopHoldsNoStoreLockDuringDaemonCalls(t *testing.T) {
	h := newHarness(t)
	h.srv.Add("volume", "v1", "myapp_data", "myapp")
	listingPending := make(chan struct{})
	releaseListing := make(chan struct{})
	var once atomic.Bool
	h.srv.OnRequest(func(method, path string) {
		if method == "GET" && strings.Contains(path, "/networks") && once.CompareAndSwap(false, true) {
			close(listingPending)
			<-releaseListing
		}
	})
	actionDone := make(chan error, 1)
	go func() {
		_, err := h.runner().StopNow(context.Background(), "myapp", "dashboard")
		actionDone <- err
	}()
	<-listingPending
	readDone := make(chan struct{})
	var readErr error
	go func() {
		defer close(readDone)
		_, readErr = h.st.Read()
	}()
	select {
	case <-readDone:
		close(releaseListing)
	case <-time.After(2 * time.Second):
		close(releaseListing)
		t.Fatal("the state file read was blocked while the daemon listing was pending")
	}
	if readErr != nil {
		t.Fatalf("Store.Read: %v", readErr)
	}
	if err := <-actionDone; err != nil {
		t.Fatalf("StopNow: %v", err)
	}
}

func TestActionsRefuseUnmanaged(t *testing.T) {
	h := newHarness(t)
	h.srv.Add("container", "x1", "stranger-web", "stranger")
	r := h.runner()
	for _, name := range []string{"stranger", "dev-lease"} {
		if _, err := r.StopNow(context.Background(), name, "dashboard"); !errors.Is(err, ErrNotManaged) {
			t.Errorf("StopNow(%s) error = %v, want ErrNotManaged", name, err)
		}
		if _, err := r.RemoveNow(context.Background(), name, RemoveOptions{}); !errors.Is(err, ErrNotManaged) {
			t.Errorf("RemoveNow(%s) error = %v, want ErrNotManaged", name, err)
		}
	}
	for _, call := range h.srv.Calls() {
		if call.Method != "GET" {
			t.Fatalf("a refused action touched the daemon: %s %s", call.Method, call.Path)
		}
	}
}

func TestActionLogLinesCarryVia(t *testing.T) {
	t.Run("StopNow lines carry the via field", func(t *testing.T) {
		h := newHarness(t)
		fullProject(h.srv)
		h.extend("myapp", time.Hour, testNow)
		if _, err := h.runner().StopNow(context.Background(), "myapp", "dashboard"); err != nil {
			t.Fatal(err)
		}
		for i, line := range h.logLines() {
			if line["via"] != "dashboard" {
				t.Errorf("line %d via = %v, want dashboard: %v", i, line["via"], line)
			}
		}
	})
	t.Run("RemoveNow lines carry the via field", func(t *testing.T) {
		h := newHarness(t)
		fullProject(h.srv)
		h.extend("myapp", time.Hour, testNow)
		if _, err := h.runner().RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"}); err != nil {
			t.Fatal(err)
		}
		for i, line := range h.logLines() {
			if line["via"] != "mcp" {
				t.Errorf("line %d via = %v, want mcp: %v", i, line["via"], line)
			}
		}
	})
	t.Run("sweep lines keep their current shape without via", func(t *testing.T) {
		h := newHarness(t)
		fullProject(h.srv)
		h.extend("myapp", time.Hour, testNow)
		h.pass(t)
		for i, line := range h.logLines() {
			if _, ok := line["via"]; ok {
				t.Errorf("line %d carries via %v, want no via field on sweep lines", i, line["via"])
			}
		}
	})
}

// Given a live stack labelled never-remove whose containers were dropped
// by a compose down (the volume keeps the project present), RemoveNow
// refuses without AllowNeverRemove even though no container is left to
// carry the label: the sweep-recorded label delay is the stack's delay,
// so the refusal leaves the state file and the daemon untouched.
func TestRemoveNowRefusesRecordedLabelZeroAfterDown(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "0",
	})
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", time.Hour, testNow)
	h.pass(t) // records LabelRemoveAfter 0 while the containers are there
	h.srv.Drop("container", "c1")

	stateBefore, err := os.ReadFile(h.st.Path)
	if err != nil {
		t.Fatal(err)
	}
	r := h.runner()
	if _, err := r.RemoveNow(context.Background(), "myapp", RemoveOptions{}); !errors.Is(err, ErrNeverRemove) {
		t.Fatalf("RemoveNow error = %v, want ErrNeverRemove", err)
	}
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("the refused removal touched the daemon")
	}
	for _, call := range h.srv.Calls() {
		if call.Method == "POST" || call.Method == "DELETE" {
			t.Fatalf("the refused removal ran %s %s", call.Method, call.Path)
		}
	}
	stateAfter, err := os.ReadFile(h.st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(stateAfter) != string(stateBefore) {
		t.Fatalf("the refused removal changed the state file:\nbefore %s\nafter %s", stateBefore, stateAfter)
	}

	// The explicit override removes the volume and clears the lease.
	rep, err := r.RemoveNow(context.Background(), "myapp", RemoveOptions{AllowNeverRemove: true})
	if err != nil {
		t.Fatalf("RemoveNow with AllowNeverRemove: %v", err)
	}
	if len(rep.Removed) != 1 {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if h.srv.Has("volume", "myapp_data") {
		t.Fatal("the allowed removal left the volume")
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared after the allowed removal")
	}
}

// Given the recorded label delay 0 with the containers already down,
// StopNow keeps it: the stop records never-remove from the recorded
// label, not from serve's setting, and a later sweep removes nothing.
func TestStopNowRecordsLabelDelayWhenDown(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "0",
	})
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", time.Hour, testNow)
	h.pass(t) // records LabelRemoveAfter 0 while the containers are there
	h.srv.Drop("container", "c1")

	if _, err := h.runner().StopNow(context.Background(), "myapp", "dashboard"); err != nil {
		t.Fatal(err)
	}
	l := h.leases()["myapp"]
	if l.State != lease.StateStopped || l.RemoveAfter == nil || *l.RemoveAfter != 0 || l.RemoveAfterFrom != "label" {
		t.Fatalf("lease after StopNow = %+v, want 0 from the recorded label", l)
	}

	// A sweep thirty days later still leaves the volume alone.
	h.cfg.Now = func() time.Time { return testNow.Add(30 * 24 * time.Hour) }
	h.pass(t)
	if !h.srv.Has("volume", "myapp_data") {
		t.Fatal("the sweep removed the never-remove volume")
	}
}

// RemoveNow of a stopped stack works from the recorded label delay even
// when the containers that carried the label are gone: the recorded delay
// was re-read from the containers while they were there, so the removal
// does not have to guess from the config.
func TestRemoveNowRemovesRecordedDelay(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "720h",
	})
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", time.Hour, testNow)
	h.pass(t)
	h.cfg.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	h.pass(t) // the stop records the container label 720h
	h.srv.Drop("container", "c1")
	if l := h.leases()["myapp"]; l.State != lease.StateStopped || l.RemoveAfter == nil || *l.RemoveAfter != 720*time.Hour {
		t.Fatalf("setup lease = %+v, want stopped with the recorded 720h", l)
	}

	r := h.runner()
	rep, err := r.RemoveNow(context.Background(), "myapp", RemoveOptions{Via: "mcp"})
	if err != nil {
		t.Fatalf("RemoveNow: %v", err)
	}
	if len(rep.Removed) != 1 || rep.Removed[0] != "myapp" {
		t.Fatalf("Removed = %v, want [myapp]", rep.Removed)
	}
	if h.srv.Has("volume", "myapp_data") {
		t.Fatal("the volume was not removed")
	}
	if _, ok := h.leases()["myapp"]; ok {
		t.Fatal("lease entry not cleared after removal")
	}
}
