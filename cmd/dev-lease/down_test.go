package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/andrius/dev-lease/internal/control"
	"github.com/andrius/dev-lease/internal/docker"
	"github.com/andrius/dev-lease/internal/fakedocker"
	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
)

// downServe is what a running serve gives the down client: a state file
// whose header carries the managed patterns, and the control socket serve
// listens on, here served straight over a fake daemon without serve's
// sweep loop.
type downServe struct {
	srv    *fakedocker.Server
	st     lease.Store
	runner *reaper.Runner
	log    *syncBuf
}

func newDownServe(t *testing.T) *downServe {
	t.Helper()
	srv := fakedocker.New(t)
	st := tempStore(t)
	writeHeader(t, st, []string{"myapp", "myapp-*"}, time.Hour)
	log := &syncBuf{}
	runner := &reaper.Runner{
		Cli:   docker.New(srv.Socket),
		Store: st,
		Cfg: reaper.Config{
			Patterns:    []string{"myapp", "myapp-*"},
			RemoveAfter: 24 * time.Hour,
			StopTimeout: time.Second,
			Log:         log,
		},
	}
	closeFn, err := control.Listen(context.Background(), control.SocketPath(st.Path), runner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeFn(); err != nil {
			t.Fatal(err)
		}
	})
	return &downServe{srv: srv, st: st, runner: runner, log: log}
}

func (h *downServe) put(t *testing.T, l lease.Lease) {
	t.Helper()
	if err := h.st.Update(func(m map[string]lease.Lease) error {
		m[l.Project] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// addSplittableStack adds a project whose removal splits kept from plain:
// a kept web service (its own labelled image, the default network and a
// kept volume it uses) and a plain db service with its own labelled image,
// plus one extra network and one plain volume that nothing keeps. The id
// prefix keeps two such stacks' ids apart on one daemon.
func addSplittableStack(srv *fakedocker.Server, project, pre string) {
	srv.AddLabels("image", "sha256:"+pre+"i1", project+"-web:latest", map[string]string{
		docker.ProjectLabel: project, docker.ServiceLabel: "web",
	})
	srv.AddContainerWithUses(pre+"c1", project+"-web-1", map[string]string{
		docker.ProjectLabel: project, docker.ServiceLabel: "web", docker.KeepLabel: "true",
	}, "sha256:"+pre+"i1", []string{project + "_default"}, []string{project + "_data"})
	srv.AddLabels("image", "sha256:"+pre+"i2", project+"-db:latest", map[string]string{
		docker.ProjectLabel: project, docker.ServiceLabel: "db",
	})
	srv.AddContainerWithUses(pre+"c2", project+"-db-1", map[string]string{
		docker.ProjectLabel: project, docker.ServiceLabel: "db",
	}, "sha256:"+pre+"i2", nil, nil)
	srv.Add("network", pre+"n1", project+"_default", project)
	srv.Add("network", pre+"n2", project+"_extra", project)
	srv.AddLabels("volume", "", project+"_data", map[string]string{
		docker.ProjectLabel: project, docker.KeepLabel: "true",
	})
	srv.Add("volume", "", project+"_tmp", project)
}

// Given a managed project with a live lease, down stops its containers and
// removes it through the removal path a sweep uses: stops precede
// deletes, what a sweep removal keeps stays untouched, everything else of
// the project goes, the lease is deleted afterwards, and serve's log
// names the holder.
func TestDownRemovesThroughTheSweepRemovalPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("when the lease is live, expect stop-then-remove with kept objects untouched", func(t *testing.T) {
		h := newDownServe(t)
		addSplittableStack(h.srv, "myapp", "p")
		now := time.Now().UTC()
		h.put(t, lease.Lease{Project: "myapp", Holders: []lease.Holder{{
			Name: "ci", ExtendedAt: now, Expires: now.Add(time.Hour), TTL: time.Hour,
		}}})

		var out bytes.Buffer
		if code := runDown(ctx, &out, h.st, "myapp", "bob"); code != 0 {
			t.Fatalf("down exit code = %d, want 0 (out=%q)", code, out.String())
		}
		if !strings.Contains(out.String(), "removed myapp") {
			t.Fatalf("output = %q, want it to say the project was removed", out.String())
		}
		if !strings.Contains(out.String(), "kept:") || !strings.Contains(out.String(), "myapp-web-1") {
			t.Fatalf("output = %q, want the kept objects named", out.String())
		}
		// What a sweep removal keeps stays: the kept container keeps
		// running, and the network, volume and image it uses stay.
		if !h.srv.Has("container", "pc1") || h.srv.IsStopped("container", "pc1") {
			t.Fatal("the kept container was stopped or removed")
		}
		if !h.srv.Has("network", "pn1") || !h.srv.Has("volume", "myapp_data") || !h.srv.Has("image", "sha256:pi1") {
			t.Fatal("the kept container's network, volume or image was removed")
		}
		// Everything else of the project goes, its built images included.
		if h.srv.Has("container", "pc2") || h.srv.Has("network", "pn2") ||
			h.srv.Has("volume", "myapp_tmp") || h.srv.Has("image", "sha256:pi2") {
			t.Fatal("a plain container, network, volume or image survived the down")
		}
		// The lease is deleted afterwards, like a sweep removal deletes it.
		leases, err := h.st.Read()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := leases["myapp"]; ok {
			t.Fatal("the lease survived the down")
		}
		// Stops precede deletes, and no stop or delete touches a kept
		// object.
		lastStop, firstDelete := -1, -1
		for i, call := range h.srv.Calls() {
			if call.Method != "POST" && call.Method != "DELETE" {
				continue
			}
			for _, kept := range []string{"pc1", "pn1", "myapp_data", "sha256:pi1"} {
				if strings.Contains(call.Path, kept) {
					t.Fatalf("a mutating call touched the kept object %s: %s %s", kept, call.Method, call.Path)
				}
			}
			switch {
			case call.Method == "POST":
				lastStop = i
			case call.Method == "DELETE" && firstDelete == -1:
				firstDelete = i
			}
		}
		if lastStop == -1 || firstDelete == -1 {
			t.Fatalf("the down ran no stop or no delete: %+v", h.srv.Calls())
		}
		if firstDelete < lastStop {
			t.Fatalf("a delete ran before the last stop: %+v", h.srv.Calls())
		}
		// The action's log lines name the holder, so serve's log says who
		// took the stack down.
		if !strings.Contains(h.log.String(), `"via":"down bob"`) {
			t.Fatalf("serve's log has no down action line:\n%s", h.log.String())
		}
	})

	t.Run("when the stack is stopped and due, expect the same daemon calls a sweep removal makes", func(t *testing.T) {
		h := newDownServe(t)
		// The twin project must match the managed patterns too, or the
		// sweep never touches it.
		addSplittableStack(h.srv, "myapp", "p")
		addSplittableStack(h.srv, "myapp-twin", "q")
		for _, id := range []string{"pc1", "pc2", "qc1", "qc2"} {
			h.srv.StopContainer(id)
		}
		stoppedDue := func(project string) lease.Lease {
			day := 24 * time.Hour
			return lease.Lease{Project: project, State: lease.StateStopped,
				StoppedAt:   time.Now().UTC().Add(-25 * time.Hour),
				RemoveAfter: &day, RemoveAfterFrom: "serve"}
		}
		h.put(t, stoppedDue("myapp"))
		h.put(t, stoppedDue("myapp-twin"))

		var out bytes.Buffer
		if code := runDown(ctx, &out, h.st, "myapp", "bob"); code != 0 {
			t.Fatalf("down exit code = %d, want 0 (out=%q)", code, out.String())
		}
		if _, err := h.runner.Pass(ctx); err != nil {
			t.Fatalf("the sweep pass that removes the twin: %v", err)
		}

		// The mutating calls of the two removals, each normalised over its
		// own ids, must be one and the same sequence: down removes through
		// the sweep's own rules and calls.
		norm := func(matches []string, pairs []string) []string {
			var seq []string
			for _, call := range h.srv.Calls() {
				if call.Method != "POST" && call.Method != "DELETE" {
					continue
				}
				hit := false
				for _, m := range matches {
					if strings.Contains(call.Path, m) {
						hit = true
						break
					}
				}
				if !hit {
					continue
				}
				path := call.Path
				for i := 0; i+1 < len(pairs); i += 2 {
					path = strings.ReplaceAll(path, pairs[i], pairs[i+1])
				}
				seq = append(seq, call.Method+" "+path)
			}
			return seq
		}
		downCalls := norm(
			[]string{"pc2", "pn2", "myapp_tmp", "sha256:pi2"},
			[]string{"pc2", "X2", "pn2", "N2", "myapp_tmp", "V2", "sha256:pi2", "I2"},
		)
		sweepCalls := norm(
			[]string{"qc2", "qn2", "myapp-twin_tmp", "sha256:qi2"},
			[]string{"qc2", "X2", "qn2", "N2", "myapp-twin_tmp", "V2", "sha256:qi2", "I2"},
		)
		if len(downCalls) == 0 || len(sweepCalls) == 0 {
			t.Fatalf("a removal ran no mutating calls: down=%v sweep=%v", downCalls, sweepCalls)
		}
		if strings.Join(downCalls, "|") != strings.Join(sweepCalls, "|") {
			t.Fatalf("the down calls differ from a sweep removal's:\ndown:  %v\nsweep: %v", downCalls, sweepCalls)
		}
	})
}

// Given a name no pattern manages, down refuses before any socket call,
// exactly like extend and release: exit 2 with a message naming the
// project.
func TestDownUnmanagedProjectExitsTwo(t *testing.T) {
	st := tempStore(t)
	writeHeader(t, st, []string{"myapp"}, time.Hour)

	var out bytes.Buffer
	if code := runDown(context.Background(), &out, st, "ghost", "bob"); code != 2 {
		t.Fatalf("down on an unmanaged project = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "ghost") || !strings.Contains(out.String(), "managed") {
		t.Fatalf("output = %q, want one naming the project and why it is refused", out.String())
	}
}

// Given a project the reaper holds mid-flight (a stop or a removal under
// way), down exits 3 and changes nothing, so a caller can retry.
func TestDownWhileTheReaperHoldsTheProjectExitsThree(t *testing.T) {
	for _, state := range []string{lease.StateStopping, lease.StateRemoving} {
		t.Run("when the lease is marked "+state+", expect exit 3", func(t *testing.T) {
			h := newDownServe(t)
			addSplittableStack(h.srv, "myapp", "p")
			h.put(t, lease.Lease{Project: "myapp", State: state})

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var out bytes.Buffer
			if code := runDown(ctx, &out, h.st, "myapp", "bob"); code != 3 {
				t.Fatalf("down on a project marked %s = %d, want 3 (out=%q)", state, code, out.String())
			}
			if !strings.Contains(out.String(), "retry") {
				t.Fatalf("output = %q, want the retry notice", out.String())
			}
			if !h.srv.Has("container", "pc2") || !h.srv.Has("network", "pn2") {
				t.Fatal("the refused down changed the daemon")
			}
		})
	}
}

// Given a managed name with nothing left anywhere (no lease, no objects
// on the daemon), down is a success with a short message: there is
// nothing to remove.
func TestDownOnNothingLeftIsZero(t *testing.T) {
	h := newDownServe(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	if code := runDown(ctx, &out, h.st, "myapp", "bob"); code != 0 {
		t.Fatalf("down on nothing left = %d, want 0 (out=%q)", code, out.String())
	}
	if !strings.Contains(out.String(), "nothing to remove") {
		t.Fatalf("output = %q, want the nothing-to-remove notice", out.String())
	}
}

// Given no running serve (the on-demand example exited), down exits 1
// with a message naming serve.
func TestDownWithoutServeExitsOne(t *testing.T) {
	st := tempStore(t)
	writeHeader(t, st, []string{"myapp"}, time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	if code := runDown(ctx, &out, st, "myapp", "bob"); code != 1 {
		t.Fatalf("down without serve = %d, want 1 (out=%q)", code, out.String())
	}
	if !strings.Contains(out.String(), "serve") {
		t.Fatalf("output = %q, want it to name serve", out.String())
	}
}

// The down command parses its flags after the positional, like extend and
// release, and reads the state path from DEV_LEASE_STATE.
func TestDownCmdParsesHolderAfterProject(t *testing.T) {
	h := newDownServe(t)
	t.Setenv("DEV_LEASE_STATE", h.st.Path)

	var out, errOut bytes.Buffer
	if code := runDownCmd([]string{"myapp", "--holder", "bob"}, &out, &errOut); code != 0 {
		t.Fatalf("down with the flag after the positional = %d, want 0 (out=%q err=%q)", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "nothing to remove") {
		t.Fatalf("output = %q, want the nothing-to-remove notice", out.String())
	}

	out.Reset()
	if code := runDownCmd([]string{}, &out, &errOut); code != 1 {
		t.Fatalf("down without a project = %d, want 1", code)
	}
}
