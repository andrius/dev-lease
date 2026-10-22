package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/andrius/dev-lease/internal/fakedocker"
)

// The removal checks that a tag still names the image it listed before it
// deletes the tag: ImageID answers the id a tag (or an id) names now.
func TestImageIDAnswersWhatATagNamesNow(t *testing.T) {
	srv := fakedocker.New(t)
	srv.AddLabels("image", "sha256:a", "myapp-web:latest", nil)
	srv.AddLabels("image", "sha256:b", "sha256:b", nil)
	srv.SetImageTags("sha256:b")
	c := New(srv.Socket)
	ctx := context.Background()

	if id, err := c.ImageID(ctx, "myapp-web:latest"); err != nil || id != "sha256:a" {
		t.Fatalf("ImageID(myapp-web:latest) = %q, %v; want sha256:a", id, err)
	}
	srv.MoveTag("myapp-web:latest", "sha256:b")
	if id, err := c.ImageID(ctx, "myapp-web:latest"); err != nil || id != "sha256:b" {
		t.Fatalf("ImageID after the tag moved = %q, %v; want sha256:b", id, err)
	}
	var se *StatusError
	if _, err := c.ImageID(ctx, "nobody-web:latest"); !errors.As(err, &se) || se.Status != 404 {
		t.Fatalf("ImageID(unknown) error = %v, want a 404 StatusError", err)
	}
}

// The image inspect by name is a read with no query; a name with a slash
// (more path segments) and any query are refused.
func TestAllowlistImageInspect(t *testing.T) {
	for _, p := range []string{
		"/v1.47/images/myapp-web:latest/json",
		"/v1.47/images/sha256:abc/json",
	} {
		if !allowed("GET", p) {
			t.Errorf("allowed(GET %s) = false, want true", p)
		}
	}
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1.47/images/myapp-web:latest/json?x=1"},
		{"GET", "/v1.47/images/team/web:1/json"},
		{"POST", "/v1.47/images/myapp-web:latest/json"},
		{"DELETE", "/v1.47/images/myapp-web:latest/json"},
		{"GET", "/v1.47/images/myapp-web:latest/history"},
	} {
		if allowed(tc.method, tc.path) {
			t.Errorf("allowed(%s %s) = true, want false", tc.method, tc.path)
		}
	}
}
