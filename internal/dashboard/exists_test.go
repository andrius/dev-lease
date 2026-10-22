package dashboard

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/lease"
)

// keep and release on a stack with no lease and nothing on the
// daemon answer 404 and change nothing (no lease is created).
func TestKeepReleaseOnEmptyName404(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))
	before, _ := os.ReadFile(h.state.Path)

	// ghost matches a pattern but has no daemon objects and no lease.
	resp := post(t, ts, "/stacks/ghost/keep", "csrf="+csrf+"&ttl=7d")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("keep on an empty stack = %d, want 404", resp.StatusCode)
	}
	resp = post(t, ts, "/stacks/ghost/release", "csrf="+csrf)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("release on an empty stack = %d, want 404", resp.StatusCode)
	}
	if _, ok := h.leases(t)["ghost"]; ok {
		t.Error("a keep on an empty stack created a lease")
	}
	after, _ := os.ReadFile(h.state.Path)
	if string(before) != string(after) {
		t.Error("an action on an empty stack changed the lease state file")
	}
}

// release on a stack with daemon objects but no lease at all (nobody
// holds it) answers with the nothing_to_release flash and creates no
// lease, instead of pretending a claim was released.
func TestReleaseWithoutLeaseSaysNothingToRelease(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))

	resp := post(t, ts, "/stacks/myapp/release", "csrf="+csrf)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/stacks/myapp?err=nothing_to_release" {
		t.Fatalf("release without a lease = %d to %q, want 303 to ?err=nothing_to_release", resp.StatusCode, resp.Header.Get("Location"))
	}
	page := body(t, get(t, ts.URL+"/stacks/myapp?err=nothing_to_release"))
	if !strings.Contains(page, "nothing to release") {
		t.Error("the nothing_to_release flash is missing")
	}
	if _, ok := h.leases(t)["myapp"]; ok {
		t.Error("the release created a lease")
	}
}

// release without a dashboard claim answers with the
// nothing_to_release flash and changes nothing; with two holders it
// ends only the dashboard claim.
func TestReleaseClaims(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.extend(t, "myapp", "agent-201", time.Hour)
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))

	t.Run("no dashboard claim: flash and nothing changes", func(t *testing.T) {
		resp := post(t, ts, "/stacks/myapp/release", "csrf="+csrf)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/stacks/myapp?err=nothing_to_release" {
			t.Fatalf("release without a claim = %d to %q, want 303 to ?err=nothing_to_release", resp.StatusCode, resp.Header.Get("Location"))
		}
		page := body(t, get(t, ts.URL+"/stacks/myapp?err=nothing_to_release"))
		if !strings.Contains(page, "nothing to release") {
			t.Error("the nothing_to_release flash is missing")
		}
		l := h.leases(t)["myapp"]
		if len(l.Holders) != 1 || l.Holders[0].Name != "agent-201" || !l.Holders[0].Expires.After(dashNow) {
			t.Errorf("the agent-201 claim changed: %+v", l.Holders)
		}
	})

	t.Run("two holders: only the dashboard claim ends", func(t *testing.T) {
		h.extend(t, "myapp", "dashboard", time.Hour)
		resp := post(t, ts, "/stacks/myapp/release", "csrf="+csrf)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/stacks/myapp?done=released" {
			t.Fatalf("release with a claim = %d to %q, want 303 to ?done=released", resp.StatusCode, resp.Header.Get("Location"))
		}
		l := h.leases(t)["myapp"]
		var piLive, dashLive bool
		for _, hd := range l.Holders {
			if !hd.Expires.After(dashNow) {
				continue
			}
			switch hd.Name {
			case "agent-201":
				piLive = true
			case "dashboard":
				dashLive = true
			}
		}
		if !piLive || dashLive {
			t.Errorf("after the release: pi live=%v dashboard live=%v, want only pi", piLive, dashLive)
		}
	})
}

// keep on a real stack still works and extends the dashboard claim.
func TestKeepOnExistingStackStillWorks(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.extend(t, "myapp", "dashboard", time.Hour)
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))
	resp := post(t, ts, "/stacks/myapp/keep", "csrf="+csrf+"&ttl=2h")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("keep = %d, want 303", resp.StatusCode)
	}
	l := h.leases(t)["myapp"]
	found := false
	for _, hd := range l.Holders {
		if hd.Name == "dashboard" && hd.Expires.After(dashNow.Add(time.Hour)) {
			found = true
		}
	}
	if !found {
		t.Errorf("the keep did not extend the dashboard claim: %+v", l.Holders)
	}
}

var _ = lease.StateStopped

// A dashboard claim that has already expired is not a claim to release:
// the answer is nothing_to_release, as with no claim at all.
func TestReleaseExpiredDashboardClaimIsNothingToRelease(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	h.extend(t, "myapp", "agent-201", time.Hour)
	if err := h.state.Update(func(m map[string]lease.Lease) error {
		_, err := lease.ExtendProject(m, "myapp", "dashboard", time.Hour, dashNow.Add(-3*time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))
	resp := post(t, ts, "/stacks/myapp/release", "csrf="+csrf)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/stacks/myapp?err=nothing_to_release" {
		t.Fatalf("release of an expired claim = %d to %q, want 303 to ?err=nothing_to_release", resp.StatusCode, resp.Header.Get("Location"))
	}
}
