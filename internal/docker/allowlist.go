package docker

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var versionPathRe = regexp.MustCompile(`^/v\d+\.\d+/`)

// allowed is the single gate every request must pass: only reads of labels
// and listings, a stop, and removes of exactly the four object kinds, with
// exactly the query parameters dev-lease itself generates. Everything else,
// including DELETE with force=1, is refused before any socket I/O.
func allowed(method, path string) bool {
	u, err := url.Parse(path)
	if err != nil {
		return false
	}
	p := u.Path
	q := u.Query()

	if method == "GET" && p == "/version" && len(q) == 0 {
		return true
	}
	m := versionPathRe.FindString(p)
	if m == "" {
		return false
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(p, m), "/"), "/")

	only := func(names ...string) bool {
		for k := range q {
			found := false
			for _, n := range names {
				if k == n {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}

	switch method {
	case "GET":
		switch {
		case len(parts) == 2 && parts[0] == "containers" && parts[1] == "json":
			// all=1 includes stopped containers; any other value is refused.
			all := q.Get("all")
			return only("filters", "all") && (all == "" || all == "1")
		case len(parts) == 3 && parts[0] == "containers" && parts[2] == "json":
			return only()
		case len(parts) == 3 && parts[0] == "containers" && parts[2] == "logs":
			// Exactly the four parameters ContainerLogs generates, with
			// tail an integer from 1 to 200 and timestamps exactly 1.
			return only("stdout", "stderr", "timestamps", "tail") &&
				q.Get("stdout") == "1" && q.Get("stderr") == "1" &&
				q.Get("timestamps") == "1" && tailInBounds(q.Get("tail"))
		case len(parts) == 3 && parts[0] == "containers" && parts[2] == "stats":
			// One-shot memory stats, no streaming.
			return only("stream", "one-shot") &&
				q.Get("stream") == "false" && q.Get("one-shot") == "true"
		case len(parts) == 2 && parts[0] == "system" && parts[1] == "df":
			return only("type") && q.Get("type") == "volume"
		case len(parts) == 1 && parts[0] == "networks":
			return only("filters")
		case len(parts) == 1 && parts[0] == "volumes":
			return only("filters")
		case len(parts) == 2 && parts[0] == "images" && parts[1] == "json":
			return only("filters")
		}
	case "POST":
		if len(parts) == 3 && parts[0] == "containers" && parts[2] == "stop" {
			t := q.Get("t")
			return only("t") && (t == "" || allDigits(t))
		}
	case "DELETE":
		switch {
		case len(parts) == 2 && parts[0] == "containers":
			// v=1 is mandatory (remove with anonymous volumes); force must
			// be absent or false, anything the daemon reads as true (1, yes,
			// true) is banned.
			force := q.Get("force")
			return only("v", "force") && q.Get("v") == "1" &&
				(force == "" || force == "0" || force == "false")
		case len(parts) == 2 && parts[0] == "networks":
			return only()
		case len(parts) == 2 && parts[0] == "volumes":
			return only()
		case len(parts) == 2 && parts[0] == "images":
			return only()
		}
	}
	return false
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// tailInBounds reports a logs tail parameter that is a decimal integer
// from 1 to 200.
func tailInBounds(s string) bool {
	if !allDigits(s) {
		return false
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 200
}
