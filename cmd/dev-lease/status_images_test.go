package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/fakedocker"
)

// status lists the images dev-lease counts as the stack's, by the same
// rule the removal uses: Compose v2 builds images without labels, so an
// unlabelled myapp-web that the web service runs from is listed, while a
// pulled image is not.
func TestStatusListsUnlabelledBuiltImage(t *testing.T) {
	st := tempStore(t)
	t.Setenv("DEV_LEASE_STATE", st.Path)
	writeHeader(t, st, []string{"myapp"}, 2*time.Hour)
	srv := fakedocker.New(t)
	srv.AddContainerWithUses("c1", "myapp-web-1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:built", nil, nil)
	srv.AddContainerWithUses("c2", "myapp-db-1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db",
	}, "sha256:pg", nil, nil)
	srv.AddLabels("image", "sha256:built", "myapp-web:latest", nil)
	srv.AddLabels("image", "sha256:pg", "postgres:16-alpine", nil)

	var out, errOut bytes.Buffer
	if code := runStatusCmd([]string{"--socket", srv.Socket}, &out, &errOut); code != 0 {
		t.Fatalf("status = %d (out=%q err=%q)", code, out.String(), errOut.String())
	}
	var line string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "myapp:") {
			line = l
		}
	}
	if !strings.Contains(line, "images=[myapp-web:latest]") {
		t.Errorf("status line = %q, want images=[myapp-web:latest]", line)
	}
}

// A pulled image stays out of status also when a kept container runs from
// it: it is not the stack's own image.
func TestStatusOmitsAPulledImageOfAKeptContainer(t *testing.T) {
	st := tempStore(t)
	t.Setenv("DEV_LEASE_STATE", st.Path)
	writeHeader(t, st, []string{"myapp"}, 2*time.Hour)
	srv := fakedocker.New(t)
	srv.AddContainerWithUses("c1", "myapp-web-1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:built", nil, nil)
	srv.AddContainerWithUses("c2", "myapp-db-1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db", "dev-lease.keep": "true",
	}, "sha256:pg", nil, nil)
	srv.AddLabels("image", "sha256:built", "myapp-web:latest", nil)
	srv.AddLabels("image", "sha256:pg", "postgres:16-alpine", nil)

	var out, errOut bytes.Buffer
	if code := runStatusCmd([]string{"--socket", srv.Socket}, &out, &errOut); code != 0 {
		t.Fatalf("status = %d (out=%q err=%q)", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "images=[myapp-web:latest]") {
		t.Errorf("status = %q, want images=[myapp-web:latest]", out.String())
	}
}

// status names a labelled image with several tags by the tag the removal
// deletes, not by whichever tag the daemon lists first.
func TestStatusNamesASeveralTagImageByTheStacksTag(t *testing.T) {
	st := tempStore(t)
	t.Setenv("DEV_LEASE_STATE", st.Path)
	writeHeader(t, st, []string{"myapp"}, 2*time.Hour)
	srv := fakedocker.New(t)
	labels := map[string]string{docker.ProjectLabel: "myapp", docker.ServiceLabel: "web"}
	srv.AddContainerWithUses("c1", "myapp-web-1", labels, "sha256:img", nil, nil)
	srv.AddLabels("image", "sha256:img", "acme/web:1", labels)
	srv.SetImageTags("sha256:img", "acme/web:1", "myapp-web:latest")

	var out, errOut bytes.Buffer
	if code := runStatusCmd([]string{"--socket", srv.Socket}, &out, &errOut); code != 0 {
		t.Fatalf("status = %d (out=%q err=%q)", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "images=[myapp-web:latest]") {
		t.Errorf("status = %q, want images=[myapp-web:latest]", out.String())
	}
}
