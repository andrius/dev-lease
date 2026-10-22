package reaper

// The services record (the com.docker.compose.service labels the sweep
// reads from the project's containers) must survive every lease
// transition the sweep performs: a stale removing mark, a revive and a
// failed refresh listing. The record is what the removal's image rule
// falls back to once no container is left, so losing it would leave the
// project's unlabelled built images behind on every removal after a
// compose down. These tests pin the record's lifecycle, mirroring the
// label-record tests of record_keep_test.go.

import (
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

// The sweep records the service labels of the project's containers while
// they are there: sorted, unique, and without a service label a container
// contributes nothing. The record follows the containers' labels and
// stays once the containers are gone.
func TestServicesRecordedSortedUniqueOnSweep(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:i1", nil, nil)
	srv.AddContainerWithUses("c2", "db", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db",
	}, "sha256:i2", nil, nil)
	srv.AddContainerWithUses("c3", "web2", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:i3", nil, nil)
	srv.AddContainerWithUses("c4", "plain", map[string]string{
		docker.ProjectLabel: "myapp",
	}, "sha256:i4", nil, nil)
	srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", 10*time.Hour, testNow)
	h.pass(t)
	if l := h.leases()["myapp"]; !slices.Equal(l.Services, []string{"db", "web"}) {
		t.Fatalf("services after the sweep = %v, want [db web] sorted and unique", l.Services)
	}

	// The record follows the containers while they are there.
	h.srv.SetContainerLabels("c2", map[string]string{docker.ServiceLabel: "api"})
	h.pass(t)
	if l := h.leases()["myapp"]; !slices.Equal(l.Services, []string{"api", "web"}) {
		t.Fatalf("services after the relabel = %v, want [api web]", l.Services)
	}

	// A compose down cannot erase the record before the stop: the removal
	// falls back to it.
	for _, id := range []string{"c1", "c2", "c3", "c4"} {
		h.srv.Drop("container", id)
	}
	h.pass(t)
	if l := h.leases()["myapp"]; !slices.Equal(l.Services, []string{"api", "web"}) {
		t.Fatalf("services after the down = %v, want [api web] kept", l.Services)
	}
}

// A stale removing mark cannot erase the services record its own stop
// read: the cleared lease's record passes to the replacement lease.
func TestStaleRemovingMarkKeepsServicesRecord(t *testing.T) {
	h := newHarness(t)
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateRemoving, Services: []string{"db", "web"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	rep := h.pass(t)
	if len(rep.Granted) != 1 || rep.Granted[0] != "myapp" {
		t.Fatalf("Granted = %v, want [myapp]", rep.Granted)
	}
	if l := h.leases()["myapp"]; !slices.Equal(l.Services, []string{"db", "web"}) {
		t.Fatalf("services after the clear and grant = %v, want the record kept", l.Services)
	}
}

// A stopped lease revived by a running container keeps the recorded
// services: the refresh's failed listing makes the inheritance observable.
func TestReviveKeepsServicesRecord(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{docker.ProjectLabel: "myapp"})
	h.srv.StopContainer("c1")
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped,
			StoppedAt: testNow.Add(-time.Hour), Services: []string{"db", "web"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A running container re-appears; the refresh listing (the third
	// containers listing of the pass) fails, so the record the pass ends
	// with is exactly what the revive inherited.
	h.srv.Add("container", "c2", "app", "myapp")
	var n atomic.Int32
	h.srv.OnRequest(func(method, path string) {
		if method == "GET" && strings.HasSuffix(path, "/containers/json") && n.Add(1) == 3 {
			h.srv.FailList("container", 500)
		}
	})
	h.pass(t)
	h.srv.OnRequest(nil)
	h.srv.UnfailList("container")

	l := h.leases()["myapp"]
	if l.State != "" {
		t.Fatalf("state = %q, want the revived live lease", l.State)
	}
	if !slices.Equal(l.Services, []string{"db", "web"}) {
		t.Fatalf("services = %v, want the record kept through the revive", l.Services)
	}
}

// A failed refresh listing keeps the recorded services and does not fail
// the pass.
func TestServicesListFailureKeepsRecord(t *testing.T) {
	h := newHarness(t)
	h.srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:i1", nil, nil)
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", time.Hour, testNow)
	h.pass(t)
	if l := h.leases()["myapp"]; !slices.Equal(l.Services, []string{"web"}) {
		t.Fatalf("setup: services = %v", l.Services)
	}

	// The refresh listing (the second containers listing of a steady
	// pass) fails: the pass completes and the record stays.
	var n atomic.Int32
	h.srv.OnRequest(func(method, path string) {
		if method == "GET" && strings.HasSuffix(path, "/containers/json") && n.Add(1) == 2 {
			h.srv.FailList("container", 500)
		}
	})
	h.pass(t)
	h.srv.OnRequest(nil)
	h.srv.UnfailList("container")

	if l := h.leases()["myapp"]; !slices.Equal(l.Services, []string{"web"}) {
		t.Fatalf("services = %v, want the record kept through the failed refresh listing", l.Services)
	}
}

// The services record is written only when it changed: a service change
// rewrites the state file, a steady pass does not.
func TestServicesChangeRewritesStateOnlyWhenChanged(t *testing.T) {
	h := newHarness(t)
	h.srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:i1", nil, nil)
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	h.extend("myapp", 10*time.Hour, testNow)
	h.pass(t)

	h.srv.SetContainerLabels("c1", map[string]string{docker.ServiceLabel: "db"})
	h.pass(t)
	if l := h.leases()["myapp"]; !slices.Equal(l.Services, []string{"db"}) {
		t.Fatalf("services = %v, want the changed record", l.Services)
	}

	before, err := os.Stat(h.st.Path)
	if err != nil {
		t.Fatal(err)
	}
	h.pass(t)
	after, err := os.Stat(h.st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("a pass with no services change rewrote the state file")
	}
}

// The first pass both grants the default lease and records the services in
// it, so a compose down before the second pass loses nothing: the removal
// still recognises the unlabelled built image.
func TestServicesRecordedOnFirstPassForNewGrant(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	srv.AddContainerWithUses("c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:unlab", nil, nil)
	srv.AddLabels("image", "sha256:unlab", "myapp-web:latest", nil)
	srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	rep := h.pass(t)
	if len(rep.Granted) != 1 || rep.Granted[0] != "myapp" {
		t.Fatalf("Granted = %v, want [myapp]", rep.Granted)
	}
	if l := h.leases()["myapp"]; !slices.Equal(l.Services, []string{"web"}) {
		t.Fatalf("services = %v, want [web] recorded on the granting pass", l.Services)
	}

	// The containers go away before any later pass; the recorded services
	// are what the removal's image rule reads.
	h.srv.Drop("container", "c1")
	h.cfg.Now = func() time.Time { return testNow.Add(3 * time.Hour) }
	h.pass(t) // the stop with no containers left
	h.ageStopped(t)
	h.pass(t) // the removal
	if h.srv.Has("image", "sha256:unlab") {
		t.Fatal("the unlabelled built image survived the removal")
	}
	if h.srv.Has("volume", "myapp_data") {
		t.Fatal("the volume survived the removal")
	}
}
