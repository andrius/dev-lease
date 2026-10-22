package lease

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func TestExtendNeverShortens(t *testing.T) {
	l, err := Extend(Lease{Project: "myapp"}, "ci", 2*time.Hour, testNow)
	if err != nil {
		t.Fatal(err)
	}
	want := testNow.Add(2 * time.Hour)
	if got := l.ExpiresAt(); !got.Equal(want) {
		t.Fatalf("ExpiresAt() = %v, want %v", got, want)
	}

	// A shorter extension must not shorten the expiry.
	later := testNow.Add(30 * time.Minute)
	l, err = Extend(l, "ci", time.Minute, later)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.ExpiresAt(); !got.Equal(want) {
		t.Fatalf("ExpiresAt() after shorter extend = %v, want %v", got, want)
	}
	if got := l.Holders[0].ExtendedAt; !got.Equal(later) {
		t.Fatalf("ExtendedAt = %v, want %v", got, later)
	}

	// A longer extension does move the expiry out.
	l, err = Extend(l, "ci", 3*time.Hour, later)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.ExpiresAt(); !got.Equal(later.Add(3 * time.Hour)) {
		t.Fatalf("ExpiresAt() after longer extend = %v, want %v", got, later.Add(3*time.Hour))
	}
}

func TestExtendRejectsTTLOutOfRange(t *testing.T) {
	for _, ttl := range []time.Duration{0, 30 * time.Second, MaxTTL + time.Minute} {
		if _, err := Extend(Lease{}, "ci", ttl, testNow); !errors.Is(err, ErrTTLOutOfRange) {
			t.Errorf("Extend(ttl=%v) error = %v, want ErrTTLOutOfRange", ttl, err)
		}
	}
	for _, ttl := range []time.Duration{MinTTL, MaxTTL} {
		if _, err := Extend(Lease{}, "ci", ttl, testNow); err != nil {
			t.Errorf("Extend(ttl=%v) error = %v, want nil", ttl, err)
		}
	}
}

func TestExpiresAtIsLatestHolder(t *testing.T) {
	l, err := Extend(Lease{}, "a", time.Hour, testNow)
	if err != nil {
		t.Fatal(err)
	}
	l, err = Extend(l, "b", 3*time.Hour, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := l.ExpiresAt(), testNow.Add(3*time.Hour); !got.Equal(want) {
		t.Fatalf("ExpiresAt() = %v, want %v", got, want)
	}
	if got := (Lease{}).ExpiresAt(); !got.IsZero() {
		t.Fatalf("ExpiresAt() with no holders = %v, want zero time", got)
	}
}

func TestReleaseOneHolderKeepsOthers(t *testing.T) {
	l, err := Extend(Lease{}, "a", time.Hour, testNow)
	if err != nil {
		t.Fatal(err)
	}
	l, err = Extend(l, "b", 2*time.Hour, testNow)
	if err != nil {
		t.Fatal(err)
	}
	releaseAt := testNow.Add(time.Minute)
	l = Release(l, "a", false, releaseAt)
	if l.Expired(releaseAt) {
		t.Fatal("lease expired, but holder b is still live")
	}
	if got, want := l.ExpiresAt(), testNow.Add(2*time.Hour); !got.Equal(want) {
		t.Fatalf("ExpiresAt() = %v, want %v", got, want)
	}
	for _, h := range l.Holders {
		if h.Name == "a" && !h.Expires.Equal(releaseAt) {
			t.Fatalf("holder a expires at %v, want release time %v", h.Expires, releaseAt)
		}
	}
}

func TestReleaseAll(t *testing.T) {
	l, err := Extend(Lease{}, "a", time.Hour, testNow)
	if err != nil {
		t.Fatal(err)
	}
	l, err = Extend(l, "b", 2*time.Hour, testNow)
	if err != nil {
		t.Fatal(err)
	}
	releaseAt := testNow.Add(time.Minute)
	l = Release(l, "", true, releaseAt)
	if !l.Expired(releaseAt) {
		t.Fatal("release --all left the lease live")
	}
	if got, want := l.ExpiresAt(), releaseAt; !got.Equal(want) {
		t.Fatalf("ExpiresAt() = %v, want %v", got, want)
	}

	// A later extend revives the lease.
	l, err = Extend(l, "ci", time.Hour, releaseAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if l.Expired(releaseAt.Add(time.Minute)) {
		t.Fatal("extend after release --all did not revive the lease")
	}
}

func TestLiveHoldersAreNeverDropped(t *testing.T) {
	l := Lease{Project: "myapp"}
	// 25 holders that expire at different moments in the past.
	for i := 0; i < 25; i++ {
		var err error
		l, err = Extend(l, fmt.Sprintf("old-%02d", i), time.Minute, testNow.Add(-2*time.Hour-time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
	}
	// One live holder and one more extend to force trimming.
	l, err := Extend(l, "live", time.Hour, testNow)
	if err != nil {
		t.Fatal(err)
	}
	l, err = Extend(l, "fresh", time.Minute, testNow)
	if err != nil {
		t.Fatal(err)
	}

	var expired, live int
	for _, h := range l.Holders {
		if h.Expires.After(testNow) {
			live++
		} else {
			expired++
		}
	}
	if live != 2 {
		t.Fatalf("live holders = %d, want 2", live)
	}
	if expired != 20 {
		t.Fatalf("expired holders kept = %d, want 20", expired)
	}
	for _, h := range l.Holders {
		if h.Name == "live" && !h.Expires.Equal(testNow.Add(time.Hour)) {
			t.Fatalf("live holder expiry = %v, want %v", h.Expires, testNow.Add(time.Hour))
		}
	}
	// The dropped ones must be the oldest expiries: old-24 was extended the
	// earliest, old-00 the latest, so trimming drops old-24 down to old-20.
	for i := 20; i < 25; i++ {
		name := fmt.Sprintf("old-%02d", i)
		if hasHolder(l, name) {
			t.Errorf("oldest expired holder %s survived trimming", name)
		}
	}
	if !hasHolder(l, "old-19") {
		t.Error("oldest kept expired holder old-19 was dropped")
	}
}

func TestReleaseUnknownHolderIsNoop(t *testing.T) {
	l, err := Extend(Lease{}, "a", time.Hour, testNow)
	if err != nil {
		t.Fatal(err)
	}
	got := Release(l, "nobody", false, testNow)
	if !got.ExpiresAt().Equal(l.ExpiresAt()) {
		t.Fatalf("ExpiresAt() = %v, want %v", got.ExpiresAt(), l.ExpiresAt())
	}
}

func hasHolder(l Lease, name string) bool {
	for _, h := range l.Holders {
		if h.Name == name {
			return true
		}
	}
	return false
}

// A stopped lease round-trips through the store with its state and
// StoppedAt intact: the reaper records them and status reads them back.
func TestStoppedStateRoundTrip(t *testing.T) {
	st := Store{Path: filepath.Join(t.TempDir(), "leases.json")}
	stoppedAt := testNow
	if err := st.Update(func(m map[string]Lease) error {
		m["myapp"] = Lease{
			Project:   "myapp",
			State:     StateStopped,
			StoppedAt: stoppedAt,
			Holders:   []Holder{{Name: "ci", Expires: testNow.Add(-time.Hour)}},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	leases, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	l, ok := leases["myapp"]
	if !ok {
		t.Fatal("lease not read back")
	}
	if l.State != StateStopped {
		t.Fatalf("state = %q, want %q", l.State, StateStopped)
	}
	if !l.StoppedAt.Equal(stoppedAt) {
		t.Fatalf("StoppedAt = %v, want %v", l.StoppedAt, stoppedAt)
	}
	if l.RemoveAfter != nil {
		t.Fatalf("RemoveAfter = %v, want nil for a lease written without it", l.RemoveAfter)
	}
}

// A stored 0 (never remove) round-trips as a non-nil 0, distinct from a
// lease without a recorded delay: the fallback to the header must only
// apply to the latter.
func TestRemoveAfterZeroRoundTrip(t *testing.T) {
	st := Store{Path: filepath.Join(t.TempDir(), "leases.json")}
	zero := time.Duration(0)
	if err := st.Update(func(m map[string]Lease) error {
		m["myapp"] = Lease{
			Project:     "myapp",
			State:       StateStopped,
			StoppedAt:   testNow,
			RemoveAfter: &zero,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	leases, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	l := leases["myapp"]
	if l.RemoveAfter == nil {
		t.Fatal("RemoveAfter = nil after the round trip, want a non-nil 0")
	}
	if *l.RemoveAfter != 0 {
		t.Fatalf("RemoveAfter = %v, want 0", *l.RemoveAfter)
	}
	if got := l.RecordedDelay(168 * time.Hour); got != 0 {
		t.Fatalf("RecordedDelay = %v, want 0: a stored 0 must not fall back", got)
	}
}

// The effective delay: the global off switch (a stored 0 or serve's
// current 0) wins; a label-set delay is the stored label value; a
// serve-set delay is the longer of the stored value and serve's current
// setting. A lease without an origin (empty remove_after_from) is
// serve-set; a lease without a recorded delay (nil) is serve-set with
// serve's current setting.
func TestEffectiveRemovalDelay(t *testing.T) {
	d := func(v time.Duration) *time.Duration { return &v }
	cases := []struct {
		name  string
		lease Lease
		serve time.Duration
		want  time.Duration
	}{
		{"serve 0 wins over a stored label delay", Lease{RemoveAfter: d(7 * 24 * time.Hour), RemoveAfterFrom: "label"}, 0, 0},
		{"serve 0 wins over a stored serve delay", Lease{RemoveAfter: d(168 * time.Hour), RemoveAfterFrom: "serve"}, 0, 0},
		{"a stored 0 wins over serve's setting", Lease{RemoveAfter: d(0), RemoveAfterFrom: "serve"}, 168 * time.Hour, 0},
		{"a stored 0 label wins over serve's setting", Lease{RemoveAfter: d(0), RemoveAfterFrom: "label"}, 168 * time.Hour, 0},
		{"a label delay is the stored value, not lengthened", Lease{RemoveAfter: d(7 * 24 * time.Hour), RemoveAfterFrom: "label"}, 720 * time.Hour, 7 * 24 * time.Hour},
		{"a label delay is the stored value, not shortened", Lease{RemoveAfter: d(7 * 24 * time.Hour), RemoveAfterFrom: "label"}, time.Hour, 7 * 24 * time.Hour},
		{"a shorter serve setting does not shorten", Lease{RemoveAfter: d(168 * time.Hour), RemoveAfterFrom: "serve"}, time.Hour, 168 * time.Hour},
		{"a longer serve setting applies", Lease{RemoveAfter: d(168 * time.Hour), RemoveAfterFrom: "serve"}, 720 * time.Hour, 720 * time.Hour},
		{"a lease without an origin is serve-set", Lease{RemoveAfter: d(168 * time.Hour)}, time.Hour, 168 * time.Hour},
		{"a lease without a recorded delay uses serve's setting", Lease{}, 168 * time.Hour, 168 * time.Hour},
		{"a lease without a recorded delay under serve 0 never removes", Lease{}, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.lease.EffectiveRemovalDelay(tc.serve); got != tc.want {
				t.Fatalf("EffectiveRemovalDelay(%v) = %v, want %v", tc.serve, got, tc.want)
			}
		})
	}
}

// The round trip keeps the origin: a label delay and a serve delay come
// back distinct.
func TestRemoveAfterFromRoundTrip(t *testing.T) {
	st := Store{Path: filepath.Join(t.TempDir(), "leases.json")}
	d := 7 * 24 * time.Hour
	if err := st.Update(func(m map[string]Lease) error {
		m["myapp"] = Lease{Project: "myapp", State: StateStopped, RemoveAfter: &d, RemoveAfterFrom: "label"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	leases, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if l := leases["myapp"]; l.RemoveAfterFrom != "label" {
		t.Fatalf("RemoveAfterFrom = %q after the round trip, want label", l.RemoveAfterFrom)
	}
}

// ExtendProject is the one place a stopped lease is revived and a claim
// extended, shared by the CLI and the dashboard.
func TestExtendProjectRevivesStopped(t *testing.T) {
	m := map[string]Lease{
		"myapp": {
			Project: "myapp", State: StateStopped, StoppedAt: testNow.Add(-time.Hour),
			RemoveAfter: durPtr(24 * time.Hour), RemoveAfterFrom: "serve",
		},
	}
	revived, err := ExtendProject(m, "myapp", "dashboard", 2*time.Hour, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !revived {
		t.Fatal("revived = false, want true for a stopped lease")
	}
	l := m["myapp"]
	if l.State != "" || !l.StoppedAt.IsZero() || l.RemoveAfter != nil || l.RemoveAfterFrom != "" {
		t.Errorf("lease = %+v, want the stopped mark cleared", l)
	}
	if l.Project != "myapp" {
		t.Errorf("Project = %q, want myapp", l.Project)
	}
	if got := l.ExpiresAt(); !got.Equal(testNow.Add(2 * time.Hour)) {
		t.Errorf("ExpiresAt() = %v, want %v", got, testNow.Add(2*time.Hour))
	}
	if len(l.Holders) != 1 || l.Holders[0].Name != "dashboard" {
		t.Errorf("Holders = %+v, want one dashboard claim", l.Holders)
	}
}

// While the reaper is mid-flight on a project an extend must refuse, not
// fight the runner.
func TestExtendProjectRefusesInFlight(t *testing.T) {
	for _, tc := range []struct {
		state string
		err   error
	}{
		{StateStopping, ErrBeingStopped},
		{StateRemoving, ErrBeingRemoved},
	} {
		m := map[string]Lease{"myapp": {Project: "myapp", State: tc.state}}
		_, err := ExtendProject(m, "myapp", "dashboard", 2*time.Hour, testNow)
		if !errors.Is(err, tc.err) {
			t.Errorf("ExtendProject on %s error = %v, want %v", tc.state, err, tc.err)
		}
		if got := m["myapp"]; len(got.Holders) != 0 {
			t.Errorf("ExtendProject on %s touched the lease: %+v", tc.state, got)
		}
	}
}

func durPtr(d time.Duration) *time.Duration { return &d }

// An extend (the lease stays live) leaves the recorded label delay
// alone: only a sweep that sees containers changes it.
func TestExtendKeepsLabelDelay(t *testing.T) {
	zero := time.Duration(0)
	l, err := Extend(Lease{Project: "myapp", LabelRemoveAfter: &zero}, "ci", time.Hour, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != 0 {
		t.Fatalf("LabelRemoveAfter after Extend = %v, want 0", l.LabelRemoveAfter)
	}
}

// The revive inside ExtendProject clears the stop's recorded delay but
// not the sweep's label record.
func TestExtendProjectKeepsLabelDelay(t *testing.T) {
	zero := time.Duration(0)
	l := Lease{
		Project:          "myapp",
		State:            StateStopped,
		StoppedAt:        testNow,
		RemoveAfter:      new(time.Duration),
		RemoveAfterFrom:  "label",
		LabelRemoveAfter: &zero,
	}
	*l.RemoveAfter = 7 * 24 * time.Hour
	m := map[string]Lease{"myapp": l}
	if revived, err := ExtendProject(m, "myapp", "ci", time.Hour, testNow); err != nil || !revived {
		t.Fatalf("ExtendProject = %v, %v; want revived without error", revived, err)
	}
	if got := m["myapp"].LabelRemoveAfter; got == nil || *got != 0 {
		t.Fatalf("LabelRemoveAfter after ExtendProject = %v, want 0: the revive clears the stop's delay, not the label record", got)
	}
	if m["myapp"].RemoveAfter != nil {
		t.Fatalf("RemoveAfter after ExtendProject = %v, want cleared by the revive", *m["myapp"].RemoveAfter)
	}
}

// A release keeps the recorded label delay: the new tenant inherits the
// same protection.
func TestReleaseKeepsLabelDelay(t *testing.T) {
	zero := time.Duration(0)
	released := Release(Lease{Project: "myapp", State: StateStopped, StoppedAt: testNow, LabelRemoveAfter: &zero}, "ci", false, testNow.Add(time.Hour))
	if released.LabelRemoveAfter == nil || *released.LabelRemoveAfter != 0 {
		t.Fatalf("LabelRemoveAfter after Release = %v, want 0", released.LabelRemoveAfter)
	}
}

func TestLabelRemoveAfterRoundTrip(t *testing.T) {
	st := Store{Path: filepath.Join(t.TempDir(), "leases.json")}
	zero := time.Duration(0)
	sevenDays := 7 * 24 * time.Hour
	if err := st.Update(func(m map[string]Lease) error {
		m["myapp"] = Lease{Project: "myapp", LabelRemoveAfter: &zero}
		m["other"] = Lease{Project: "other", LabelRemoveAfter: &sevenDays}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	leases, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if l := leases["myapp"]; l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != 0 {
		t.Fatalf("myapp LabelRemoveAfter = %v, want the 0 round-tripped", l.LabelRemoveAfter)
	}
	if l := leases["other"]; l.LabelRemoveAfter == nil || *l.LabelRemoveAfter != sevenDays {
		t.Fatalf("other LabelRemoveAfter = %v, want 7d round-tripped", l.LabelRemoveAfter)
	}
}
