package dashboard

import (
	"net/http"
	"strings"
	"testing"
)

// a flash shows a query-carried stack name only when the serve
// config manages it; injected text never appears.
func TestFlashNamesOnlyManagedOrOwn(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{})
	page := body(t, get(t, ts.URL+"/?done=removed&stack=prod-db+and+its+backups.+Your+session+expired,+log+in+at+evil.example"))
	for _, part := range []string{"prod-db", "its backups", "session expired", "evil.example", "log in at"} {
		if strings.Contains(page, part) {
			t.Errorf("the flash repeats %q:\n%s", part, page)
		}
	}
	// A managed name in the query is shown.
	page = body(t, get(t, ts.URL+"/?done=removed&stack=myapp"))
	if !strings.Contains(page, "Removed myapp.") {
		t.Errorf("the flash does not show the managed name:\n%s", page)
	}
}

// error pages never repeat URL text that fails reaper.Managed.
func TestErrorPagesNeverRepeatUnmanagedNames(t *testing.T) {
	h := newDH(t)
	h.fake.Add("container", "c1", "myapp_web_1", "myapp")
	ts := h.serve(t, Options{})
	csrf := csrfFrom(t, body(t, get(t, ts.URL+"/stacks/myapp")))

	cases := []struct{ path, form string }{
		{"/stacks/evil%2Ecom", ""},
		{"/stacks/evil%2Ecom/keep", "csrf=" + csrf + "&ttl=2h"},
		{"/stacks/evil%2Ecom/release", "csrf=" + csrf},
		{"/stacks/evil%2Ecom/stop", "csrf=" + csrf},
		{"/stacks/evil%2Ecom/remove", "csrf=" + csrf + "&confirm=evil.com"},
	}
	for _, tc := range cases {
		method := http.MethodGet
		form := ""
		if tc.form != "" {
			method = http.MethodPost
			form = tc.form
		}
		req, err := http.NewRequest(method, ts.URL+tc.path, strings.NewReader(form))
		if err != nil {
			t.Fatal(err)
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", ts.URL)
		}
		resp, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		page := body(t, resp)
		if strings.Contains(page, "evil.com") {
			t.Errorf("%s %s repeats the unmanaged name:\n%s", method, tc.path, page)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", method, tc.path, resp.StatusCode)
		}
	}
	// The remove confirm mismatch on a managed name keeps the typed-name
	// message (the name is the page's own path value).
	resp := post(t, ts, "/stacks/myapp/remove", "csrf="+csrf+"&confirm=not-it")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("remove with a mismatched name = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(body(t, resp), "type myapp to confirm") {
		t.Error("the mismatch refusal does not name the stack")
	}
}
