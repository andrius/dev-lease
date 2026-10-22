package fakedocker

import (
	"encoding/json"
	"slices"
	"testing"
)

// The fake deletes images the way the daemon does without force: an id
// whose tags span several repositories is refused, a tag shared with
// other tags is only untagged, and the last tag goes with the image
// unless a container uses it.
func TestDeleteImageFollowsTheDaemonsTagRules(t *testing.T) {
	newShared := func(t *testing.T) (*Server, func(string) ([]byte, int)) {
		s := New(t)
		s.AddLabels("image", "sha256:shared", "myapp-web:latest", nil)
		s.SetImageTags("sha256:shared", "myapp-web:latest", "other-web:latest")
		hc := client(t, s.Socket)
		return s, func(ref string) ([]byte, int) { return del(t, hc, "/v1.47/images/"+ref) }
	}
	tagsOf := func(t *testing.T, s *Server) []string {
		t.Helper()
		body, status := get(t, client(t, s.Socket), "/v1.47/images/json")
		if status != 200 {
			t.Fatalf("GET images = %d", status)
		}
		var imgs []struct {
			Id       string
			RepoTags []string
		}
		if err := json.Unmarshal(body, &imgs); err != nil {
			t.Fatal(err)
		}
		for _, i := range imgs {
			if i.Id == "sha256:shared" {
				return i.RepoTags
			}
		}
		return nil
	}

	t.Run("when the id is deleted and its tags span two repositories, expect 409 and both tags kept", func(t *testing.T) {
		s, del := newShared(t)
		if _, status := del("sha256:shared"); status != 409 {
			t.Fatalf("DELETE by id = %d, want 409", status)
		}
		if got := tagsOf(t, s); !slices.Equal(got, []string{"myapp-web:latest", "other-web:latest"}) {
			t.Fatalf("tags = %v, want both", got)
		}
	})
	t.Run("when one of two tags is deleted, expect it untagged and the image kept", func(t *testing.T) {
		s, del := newShared(t)
		if _, status := del("myapp-web:latest"); status != 200 {
			t.Fatalf("DELETE by tag = %d, want 200", status)
		}
		if got := tagsOf(t, s); !slices.Equal(got, []string{"other-web:latest"}) {
			t.Fatalf("tags = %v, want [other-web:latest]", got)
		}
		if !s.Has("image", "sha256:shared") {
			t.Fatal("the image went with a tag it shared")
		}
	})
	t.Run("when the last tag is deleted, expect the image gone", func(t *testing.T) {
		s, del := newShared(t)
		del("myapp-web:latest")
		if _, status := del("other-web:latest"); status != 200 {
			t.Fatalf("DELETE last tag = %d, want 200", status)
		}
		if s.Has("image", "sha256:shared") {
			t.Fatal("the image survived its last tag")
		}
	})
	t.Run("when a container uses the image, expect a shared tag untagged but the last tag refused", func(t *testing.T) {
		s, del := newShared(t)
		s.AddContainerWithImage("c1", "web", "other", "sha256:shared")
		if _, status := del("myapp-web:latest"); status != 200 {
			t.Fatalf("DELETE shared tag of an image in use = %d, want 200", status)
		}
		if _, status := del("other-web:latest"); status != 409 {
			t.Fatalf("DELETE last tag of an image in use = %d, want 409", status)
		}
		if got := tagsOf(t, s); !slices.Equal(got, []string{"other-web:latest"}) {
			t.Fatalf("tags = %v, want [other-web:latest]", got)
		}
	})
	t.Run("when an unknown tag is deleted, expect 404", func(t *testing.T) {
		_, del := newShared(t)
		if _, status := del("nobody-web:latest"); status != 404 {
			t.Fatalf("DELETE unknown tag = %d, want 404", status)
		}
	})
	t.Run("when a single-tag image is deleted by its tag, expect the image gone", func(t *testing.T) {
		s := New(t)
		s.Add("image", "sha256:one", "myapp-web:latest", "myapp")
		if _, status := del(t, client(t, s.Socket), "/v1.47/images/myapp-web:latest"); status != 200 {
			t.Fatalf("DELETE by tag = %d, want 200", status)
		}
		if s.Has("image", "sha256:one") {
			t.Fatal("the image survived the delete of its only tag")
		}
	})
}
