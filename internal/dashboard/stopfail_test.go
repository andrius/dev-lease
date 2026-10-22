package dashboard

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// When the daemon fails to stop some of the containers (a 500 answer or
// a 409 conflict), the action still redirects to the stack page, but
// with the not_stopped flash that names the stack and points at the
// serve log, and the lease state stays put.
func TestStopFailureRedirectsNotStopped(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  string
		code int
	}{
		{name: "daemon answers 500", ref: "c2", code: 500},
		{name: "daemon answers 409", ref: "c2", code: 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDH(t)
			h.fake.Add("container", "c1", "myapp_web_1", "myapp")
			h.fake.Add("container", "c2", "myapp_db_1", "myapp")
			h.fake.RefuseStop("container", tc.ref, tc.code)
			h.extend(t, "myapp", "agent-201", time.Hour)
			ts := h.serve(t, Options{})
			csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))

			resp := post(t, ts, "/stacks/myapp/stop", "csrf="+csrf)
			if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/stacks/myapp?err=not_stopped" {
				t.Fatalf("stop with a failing daemon = %d to %q, want 303 to ?err=not_stopped", resp.StatusCode, resp.Header.Get("Location"))
			}
			page := body(t, get(t, ts.URL+"/stacks/myapp?err=not_stopped"))
			if !strings.Contains(page, "Some containers of myapp could not be stopped; see serve&#39;s log.") {
				t.Error("the not_stopped flash is missing")
			}
			stopped := map[string]bool{}
			for _, c := range h.fake.Calls() {
				if c.Method == http.MethodPost && strings.Contains(c.Path, "/stop") {
					stopped[c.Path] = true
				}
			}
			if !stopped["/v1.47/containers/c1/stop"] {
				t.Errorf("the healthy container was not stopped: %v", stopped)
			}
			if !h.fake.IsStopped("container", "c1") {
				t.Error("the healthy container still runs after the stop")
			}
			if h.fake.IsStopped("container", "c2") {
				t.Error("the refused container was stopped")
			}
		})
	}
}
