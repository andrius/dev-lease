package docker

import "testing"

// TestAllowlistNewReadCalls pins the three new read-only calls the dashboard
// and the MCP tools use: container logs, memory stats and volume sizes. Only
// the exact calls dev-lease itself generates pass; any extra query key, a
// different value, or a mutation on the same path is refused.
func TestAllowlistNewReadCalls(t *testing.T) {
	permitted := []struct {
		method, path string
	}{
		{"GET", "/v1.47/containers/abc/logs?stdout=1&stderr=1&timestamps=1&tail=200"},
		{"GET", "/v1.47/containers/abc/logs?tail=1&timestamps=1&stderr=1&stdout=1"},
		{"GET", "/v1.47/containers/abc/stats?stream=false&one-shot=true"},
		{"GET", "/v1.47/system/df?type=volume"},
	}
	for _, tc := range permitted {
		if !allowed(tc.method, tc.path) {
			t.Errorf("allowed(%s %s) = false, want true", tc.method, tc.path)
		}
	}

	refused := []struct {
		method, path string
	}{
		{"GET", "/v1.47/containers/abc/logs?stdout=1&stderr=1&timestamps=1&tail=5&follow=1"},
		{"GET", "/v1.47/containers/abc/logs?stdout=1&stderr=1&timestamps=1"},
		{"GET", "/v1.47/containers/abc/logs?stdout=1&stderr=1&timestamps=1&tail=5&since=1698083677"},
		{"GET", "/v1.47/containers/abc/stats?stream=true&one-shot=true"},
		{"GET", "/v1.47/containers/abc/stats?stream=false"},
		{"GET", "/v1.47/containers/abc/stats?stream=false&one-shot=true&type=cpu"},
		{"GET", "/v1.47/system/df?type=image"},
		{"GET", "/v1.47/system/df"},
		{"POST", "/v1.47/containers/abc/logs?stdout=1&stderr=1&timestamps=1&tail=5"},
		{"POST", "/v1.47/containers/abc/stats?stream=false&one-shot=true"},
		{"POST", "/v1.47/system/df?type=volume"},
	}
	for _, tc := range refused {
		if allowed(tc.method, tc.path) {
			t.Errorf("allowed(%s %s) = true, want false", tc.method, tc.path)
		}
	}
}

// The logs tail must be an integer from 1 to 200 and timestamps exactly 1.
func TestAllowlistLogsTailAndTimestamps(t *testing.T) {
	for _, p := range []string{
		"/v1.47/containers/abc/logs?stdout=1&stderr=1&timestamps=1&tail=1",
		"/v1.47/containers/abc/logs?stdout=1&stderr=1&timestamps=1&tail=200",
	} {
		if !allowed("GET", p) {
			t.Errorf("allowed(GET %s) = false, want true", p)
		}
	}
	for _, p := range []string{
		"/v1.47/containers/abc/logs?stdout=1&stderr=1&timestamps=1&tail=0",
		"/v1.47/containers/abc/logs?stdout=1&stderr=1&timestamps=1&tail=201",
		"/v1.47/containers/abc/logs?stdout=1&stderr=1&timestamps=0&tail=5",
		"/v1.47/containers/abc/logs?stdout=1&stderr=1&timestamps=2&tail=5",
	} {
		if allowed("GET", p) {
			t.Errorf("allowed(GET %s) = true, want false", p)
		}
	}
}
