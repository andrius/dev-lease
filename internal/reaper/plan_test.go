package reaper

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/docker"
)

// TestPlanForMatchesEngineActions pins PlanFor to what the engine really
// does. For several stacks the plan is computed from listings taken
// before anything runs, then the real passes run: the first stops the
// expired projects and the second removes them. The stopped containers
// must be exactly plan.Stop and the deleted objects exactly the plan's
// removal lists, with everything kept by label or kept use still there.
func TestPlanForMatchesEngineActions(t *testing.T) {
	h := newHarness(t)
	srv := h.srv

	// Project "other": a kept service with its volume and network, a kept
	// volume, a pulled image, a built image and an image built FROM a
	// project image.
	srv.AddContainerWithUses("o-web", "other_web_1", map[string]string{
		docker.ProjectLabel: "other", docker.ServiceLabel: "web",
	}, "sha256:kuimg", []string{"other_default"}, []string{"web_data"})
	srv.AddContainerWithUses("o-db", "other_db_1", map[string]string{
		docker.ProjectLabel: "other", docker.ServiceLabel: "db", docker.KeepLabel: "true",
	}, "sha256:pg16", []string{"other_default"}, []string{"db_data"})
	srv.SetContainerImageRef("o-db", "postgres:16-alpine")
	srv.Add("network", "n-other", "other_default", "other")
	srv.Add("volume", "v-web", "web_data", "other")
	srv.Add("volume", "v-db", "db_data", "other")
	srv.AddLabels("volume", "v-keep", "keep_me", map[string]string{
		docker.ProjectLabel: "other", docker.KeepLabel: "true",
	})
	srv.AddLabels("image", "sha256:kuimg", "other-web:latest", map[string]string{
		docker.ProjectLabel: "other", docker.ServiceLabel: "web",
	})
	// Built FROM a project image: it inherits the project label but its
	// tag does not match its service, and no container runs from it.
	srv.AddLabels("image", "sha256:inherit", "other-builder:dev", map[string]string{
		docker.ProjectLabel: "other", docker.ServiceLabel: "web",
	})
	// The pulled image carries no project label: it is never listed and
	// never removed.
	srv.AddLabels("image", "sha256:pg16", "postgres:16-alpine", nil)

	// Project "myapp": a plain stack; everything goes, including the
	// unlabelled built images Compose v2 leaves.
	srv.AddContainerWithUses("p-web", "myapp_web_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	}, "sha256:pimg", nil, nil)
	srv.AddContainerWithUses("p-db", "myapp_db_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db",
	}, "sha256:pimg", nil, nil)
	// p-old exited on its own: the stop plan must not include it, the
	// removal must still delete it.
	srv.AddContainerWithUses("p-old", "myapp_old_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "old",
	}, "sha256:pimg", nil, nil)
	srv.StopContainer("p-old")
	// p-worker runs from an unlabelled built image (Compose v2 builds
	// images without labels); p-db2 runs from a pulled image.
	srv.AddContainerWithUses("p-worker", "myapp_worker_1", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "worker",
	}, "sha256:unlab", nil, nil)
	srv.AddContainerWithUses("p-db2", "myapp_db_2", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "db",
	}, "sha256:pg2", nil, nil)
	srv.SetContainerImageRef("p-db2", "postgres:16-alpine")
	srv.Add("network", "n-myapp", "myapp_default", "myapp")
	srv.Add("volume", "v-myapp", "myapp_data", "myapp")
	srv.AddLabels("image", "sha256:pimg", "myapp-web:latest", map[string]string{
		docker.ProjectLabel: "myapp", docker.ServiceLabel: "web",
	})
	// The unlabelled built images: one a container runs from, one with no
	// container left (the service moved on), and the pulled image p-db2
	// runs from.
	srv.AddLabels("image", "sha256:unlab", "myapp-worker:latest", nil)
	srv.AddLabels("image", "sha256:left", "myapp-db:latest", nil)
	srv.AddLabels("image", "sha256:pg2", "postgres:16-alpine", nil)
	// An image labelled for another project but tagged like myapp's web
	// image: neither removal may touch it.
	srv.AddLabels("image", "sha256:xlab", "myapp-web:latest", map[string]string{
		docker.ProjectLabel: "other", docker.ServiceLabel: "web",
	})

	cli := docker.New(srv.Socket)
	ctx := context.Background()

	// snapshot is one project's listings before anything ran, and the
	// plan computed from them.
	type snapshot struct {
		plan    Plan
		initial map[string][]string
		// runningBefore are the containers that ran when the snapshot
		// was taken; the pass must stop exactly the non-kept ones.
		runningBefore []string
	}
	snap := func(project string) snapshot {
		t.Helper()
		conts, err := cli.Containers(ctx, project)
		if err != nil {
			t.Fatalf("%s: containers: %v", project, err)
		}
		nets, err := cli.Networks(ctx, project)
		if err != nil {
			t.Fatalf("%s: networks: %v", project, err)
		}
		vols, err := cli.Volumes(ctx, project)
		if err != nil {
			t.Fatalf("%s: volumes: %v", project, err)
		}
		imgs, err := cli.Images(ctx, "")
		if err != nil {
			t.Fatalf("%s: images: %v", project, err)
		}
		names := func(objs []docker.Object) []string {
			out := make([]string, 0, len(objs))
			for _, o := range objs {
				out = append(out, o.Name)
			}
			slices.Sort(out)
			return out
		}
		var runningBefore []string
		for _, c := range conts {
			if c.Running {
				runningBefore = append(runningBefore, c.Name)
			}
		}
		slices.Sort(runningBefore)
		return snapshot{
			plan: PlanFor(project, ServicesOf(conts, nil), conts, nets, vols, imgs),
			initial: map[string][]string{
				"container": names(conts),
				"network":   names(nets),
				"volume":    names(vols),
			},
			runningBefore: runningBefore,
		}
	}
	other := snap("other")
	myapp := snap("myapp")

	// The plans say exactly what they should, before anything ran.
	assertPlan := func(project string, got, want Plan) {
		t.Helper()
		for _, field := range []struct {
			name string
			got  []string
			want []string
		}{
			{"Stop", got.Stop, want.Stop},
			{"Containers", got.Containers, want.Containers},
			{"Networks", got.Networks, want.Networks},
			{"Volumes", got.Volumes, want.Volumes},
			{"Images", got.Images, want.Images},
			{"Kept", got.Kept, want.Kept},
		} {
			if !slices.Equal(field.got, field.want) {
				t.Errorf("%s plan %s = %v, want %v", project, field.name, field.got, field.want)
			}
		}
	}
	assertPlan("other", other.plan, Plan{
		Stop:       []string{"other_web_1"},
		Containers: []string{"other_web_1"},
		Networks:   []string{},
		Volumes:    []string{"web_data"},
		Images:     []string{"other-web:latest"},
		Kept:       []string{"db_data", "keep_me", "other_db_1", "other_default", "postgres:16-alpine"},
	})
	assertPlan("myapp", myapp.plan, Plan{
		Stop:       []string{"myapp_db_1", "myapp_db_2", "myapp_web_1", "myapp_worker_1"},
		Containers: []string{"myapp_db_1", "myapp_db_2", "myapp_old_1", "myapp_web_1", "myapp_worker_1"},
		Networks:   []string{"myapp_default"},
		Volumes:    []string{"myapp_data"},
		Images:     []string{"myapp-db:latest", "myapp-web:latest", "myapp-worker:latest"},
		Kept:       []string{},
	})

	// The stop: both leases expire, one pass stops both projects.
	h.extend("other", time.Minute, testNow.Add(-2*time.Minute))
	h.extend("myapp", time.Minute, testNow.Add(-2*time.Minute))
	rep := h.pass(t)
	if len(rep.Removed) != 0 {
		t.Fatalf("the expiry pass removed %v, want a stop only", rep.Removed)
	}
	for _, tc := range []struct {
		project       string
		plan          Plan
		runningBefore []string
	}{
		{"other", other.plan, other.runningBefore},
		{"myapp", myapp.plan, myapp.runningBefore},
	} {
		conts, err := cli.Containers(ctx, tc.project)
		if err != nil {
			t.Fatal(err)
		}
		// The pass stops exactly the non-kept containers that ran
		// before it; the kept ones keep running; a container that had
		// already exited is neither in plan.Stop nor stopped.
		byName := map[string]docker.Object{}
		for _, c := range conts {
			byName[c.Name] = c
		}
		var stopped []string
		for _, c := range conts {
			if !c.Running && slices.Contains(tc.plan.Stop, c.Name) {
				stopped = append(stopped, c.Name)
			}
		}
		slices.Sort(stopped)
		wantStopped := slices.Clone(tc.plan.Stop)
		slices.Sort(wantStopped)
		if !slices.Equal(stopped, wantStopped) {
			t.Errorf("%s: the stop stopped %v, want exactly plan.Stop %v", tc.project, stopped, wantStopped)
		}
		for _, name := range tc.runningBefore {
			c, ok := byName[name]
			if !ok {
				t.Errorf("%s: %s vanished during the pass", tc.project, name)
				continue
			}
			if docker.Kept(c) {
				if !c.Running {
					t.Errorf("%s: the kept container %s was stopped", tc.project, name)
				}
				continue
			}
			if !slices.Contains(tc.plan.Stop, name) {
				t.Errorf("%s: %s ran before the pass and is not kept, but plan.Stop does not include it", tc.project, name)
			}
			if c.Running {
				t.Errorf("%s: %s still runs after the pass although plan.Stop includes it", tc.project, name)
			}
		}
		// An exited non-kept container never enters the stop plan.
		for _, name := range tc.plan.Stop {
			if !slices.Contains(tc.runningBefore, name) {
				t.Errorf("%s: plan.Stop includes %s which did not run at the snapshot", tc.project, name)
			}
		}
	}
	conts, err := cli.Containers(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range conts {
		if c.Name == "other_db_1" && !c.Running {
			t.Error("the kept container was stopped")
		}
	}

	// The removal: backdate the stops past the delay; one pass removes
	// both projects.
	h.ageStopped(t)
	rep = h.pass(t)
	if len(rep.Removed) != 2 {
		t.Fatalf("Removed = %v, want both projects", rep.Removed)
	}
	for _, tc := range []struct {
		project string
		sn      snapshot
	}{
		{"other", other},
		{"myapp", myapp},
	} {
		remove := map[string][]string{
			"container": tc.sn.plan.Containers,
			"network":   tc.sn.plan.Networks,
			"volume":    tc.sn.plan.Volumes,
		}
		for _, kind := range []string{"container", "network", "volume"} {
			del := map[string]bool{}
			for _, n := range remove[kind] {
				del[n] = true
			}
			var want []string
			for _, n := range tc.sn.initial[kind] {
				if !del[n] {
					want = append(want, n)
				}
			}
			remain, err := listNames(ctx, cli, kind, tc.project)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(remain, want) {
				t.Errorf("%s: %ss left after the removal = %v, want exactly %v (the plan deleted %v)", tc.project, kind, remain, want, remove[kind])
			}
		}
	}
	// The removal lists images unfiltered (the unlabelled Compose v2 built
	// images are in the plan) and one tag name can sit on several images,
	// so the surviving images are pinned id by id.
	if srv.Count("image") != 4 {
		t.Fatalf("images left = %d, want 4", srv.Count("image"))
	}
	for _, w := range []struct {
		id   string
		gone bool
	}{
		{"sha256:kuimg", true},    // other's built image, a container ran from it
		{"sha256:inherit", false}, // built FROM a project image: the tag does not match, no container runs from it
		{"sha256:pg16", false},    // the pulled image a kept container runs from
		{"sha256:pimg", true},     // myapp's built image, three containers ran from it
		{"sha256:unlab", true},    // myapp's unlabelled built image, a container ran from it
		{"sha256:left", true},     // myapp's unlabelled built image, no container left: the db tag matches the db service
		{"sha256:pg2", false},     // the pulled image a non-kept container runs from
		{"sha256:xlab", false},    // labelled for other, tagged like myapp's web image
	} {
		if got := srv.Has("image", w.id); got == w.gone {
			t.Errorf("image %s gone=%v, want gone=%v", w.id, got, w.gone)
		}
	}
}

// listNames lists one object kind of one project and returns its names
// sorted.
func listNames(ctx context.Context, cli *docker.Client, kind, project string) ([]string, error) {
	var objs []docker.Object
	var err error
	switch kind {
	case "container":
		objs, err = cli.Containers(ctx, project)
	case "network":
		objs, err = cli.Networks(ctx, project)
	case "volume":
		objs, err = cli.Volumes(ctx, project)
	case "image":
		objs, err = cli.Images(ctx, project)
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(objs))
	for _, o := range objs {
		names = append(names, o.Name)
	}
	slices.Sort(names)
	return names, nil
}
