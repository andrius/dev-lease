package lease

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStoreUpdateReadRoundTrip(t *testing.T) {
	st := Store{Path: filepath.Join(t.TempDir(), "leases.json")}
	err := st.Update(func(m map[string]Lease) error {
		l, err := Extend(Lease{Project: "myapp"}, "ci", 2*time.Hour, testNow)
		if err != nil {
			return err
		}
		m["myapp"] = l
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	leases, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	l, ok := leases["myapp"]
	if !ok {
		t.Fatal("lease not found after round trip")
	}
	if got, want := l.ExpiresAt(), testNow.Add(2*time.Hour); !got.Equal(want) {
		t.Fatalf("ExpiresAt() = %v, want %v", got, want)
	}
	// No temp files may be left behind.
	entries, err := os.ReadDir(filepath.Dir(st.Path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	st := Store{Path: filepath.Join(t.TempDir(), "leases.json")}
	want := Header{Patterns: []string{"myapp", "myapp-*"}, DefaultTTL: 2 * time.Hour}
	if err := st.UpdateHeader(func(h *Header) error {
		*h = want
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ReadHeader()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Patterns) != 2 || got.Patterns[0] != "myapp" || got.DefaultTTL != 2*time.Hour {
		t.Fatalf("ReadHeader() = %+v, want %+v", got, want)
	}
}

func TestReadMissingFileIsEmpty(t *testing.T) {
	st := Store{Path: filepath.Join(t.TempDir(), "leases.json")}
	leases, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("Read() on missing file = %v, want empty map", leases)
	}
}

func TestCorruptFileIsNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.json")
	garbage := []byte("{ this is not json\n")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	st := Store{Path: path}
	err := st.Update(func(m map[string]Lease) error {
		m["x"] = Lease{Project: "x"}
		return nil
	})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Update() error = %v, want ErrCorrupt", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, garbage) {
		t.Fatal("corrupt file was overwritten")
	}
	if _, err := st.Read(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Read() error = %v, want ErrCorrupt", err)
	}
}

func TestConcurrentUpdatesKeepBothHolders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.json")
	st := Store{Path: path}
	if err := st.Update(func(m map[string]Lease) error {
		l, err := Extend(Lease{Project: "proj"}, "seed", time.Hour, testNow)
		if err != nil {
			return err
		}
		m["proj"] = l
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Every writer claims the lease with its own unique holder name, so a
	// lost read-modify-write cycle loses a whole holder, not just a TTL
	// bump. With the lock working, all of them must survive.
	const n = 50
	start := time.Now().UTC()
	var wg sync.WaitGroup
	barrier := make(chan struct{})
	workers := make([]string, 0, n+2)
	for i := 0; i < n; i++ {
		workers = append(workers, fmt.Sprintf("goroutine-%d", i))
	}
	workers = append(workers, "subprocess-a", "subprocess-b")

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(holder string) {
			defer wg.Done()
			<-barrier
			if err := st.Update(func(m map[string]Lease) error {
				l, err := Extend(m["proj"], holder, 2*time.Hour, time.Now().UTC())
				if err != nil {
					return err
				}
				m["proj"] = l
				return nil
			}); err != nil {
				t.Errorf("extend %s: %v", holder, err)
			}
		}(workers[i])
	}

	// Two subprocesses extend their own holders concurrently.
	cmds := make([]*exec.Cmd, 0, 2)
	for _, holder := range []string{"subprocess-a", "subprocess-b"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		cmd.Env = append(os.Environ(),
			"DEV_LEASE_HELPER=1",
			"DEV_LEASE_STATE="+path,
			"DEV_LEASE_HOLDER="+holder,
		)
		cmds = append(cmds, cmd)
	}
	for _, cmd := range cmds {
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	close(barrier)
	wg.Wait()
	for _, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper process: %v", err)
		}
	}

	leases, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	l, ok := leases["proj"]
	if !ok {
		t.Fatal("lease vanished")
	}
	// Every unique live holder survived: nothing was lost to a torn
	// read-modify-write cycle.
	live := 0
	for _, h := range l.Holders {
		if h.Expires.After(start) {
			live++
		}
	}
	if want := n + 2; live != want {
		t.Fatalf("live holders = %d, want all %d unique writers", live, want)
	}
	for _, holder := range workers {
		found := false
		for _, h := range l.Holders {
			if h.Name == holder {
				found = true
				if h.Expires.Before(start.Add(2 * time.Hour)) {
					t.Fatalf("holder %s expires at %v, want after %v", holder, h.Expires, start.Add(2*time.Hour))
				}
			}
		}
		if !found {
			t.Fatalf("holder %s was lost to a concurrent update", holder)
		}
	}
}

// TestHelperProcess is the subprocess half of
// TestConcurrentUpdatesKeepBothHolders: it extends one holder n times and
// exits.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("DEV_LEASE_HELPER") != "1" {
		t.Skip("helper process for TestConcurrentUpdatesKeepBothHolders")
	}
	st := Store{Path: os.Getenv("DEV_LEASE_STATE")}
	holder := os.Getenv("DEV_LEASE_HOLDER")
	for i := 0; i < 50; i++ {
		if err := st.Update(func(m map[string]Lease) error {
			l, err := Extend(m["proj"], holder, 2*time.Hour, time.Now().UTC())
			if err != nil {
				return err
			}
			m["proj"] = l
			return nil
		}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(0)
}
