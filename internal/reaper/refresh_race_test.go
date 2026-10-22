package reaper

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/lease"
)

// The refresh reads the leases at pass start and writes its merged delay
// back after listing the containers. A developer extend landing between
// the read and the write turns the stopped lease live: the refresh's
// store update must not write the stopped lease's merged delay onto the
// live lease.
func TestPassRefreshRaceExtendKeepsLiveLease(t *testing.T) {
	h := newHarness(t)
	h.srv.AddLabels("container", "c1", "web", map[string]string{
		docker.ProjectLabel: "myapp", docker.RemoveAfterLabel: "10d",
	})
	h.srv.StopContainer("c1")
	h.srv.Add("volume", "myapp_data", "myapp_data", "myapp")
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m["myapp"] = lease.Lease{Project: "myapp", State: lease.StateStopped, StoppedAt: testNow}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The extend lands during the first container listing of the pass,
	// after the pass read the leases and before the refresh's update.
	var once sync.Once
	h.srv.OnRequest(func(method, path string) {
		if method != "GET" || !strings.HasSuffix(path, "/containers/json") {
			return
		}
		once.Do(func() {
			if err := h.st.Update(func(m map[string]lease.Lease) error {
				revived, err := lease.ExtendProject(m, "myapp", "bob", 4*time.Hour, testNow)
				if err != nil {
					return err
				}
				if !revived {
					return errors.New("the extend did not revive the stopped lease")
				}
				return nil
			}); err != nil {
				t.Errorf("extend in the hook: %v", err)
			}
		})
	})
	h.pass(t)

	l := h.leases()["myapp"]
	if l.State != "" {
		t.Fatalf("state = %q, want live", l.State)
	}
	if l.RemoveAfter != nil || l.RemoveAfterFrom != "" {
		t.Fatalf("the refresh wrote the stopped lease's delay onto the live lease: %+v", l)
	}
	if len(l.Holders) != 1 || l.Holders[0].Name != "bob" || !l.Holders[0].Expires.Equal(testNow.Add(4*time.Hour)) {
		t.Fatalf("holders = %+v, want bob for 4h", l.Holders)
	}
}
