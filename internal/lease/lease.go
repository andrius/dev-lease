// Package lease implements the dev-lease state: per-project TTL leases with
// named holders, persisted in one JSON file.
package lease

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// MinTTL and MaxTTL bound the time to live accepted by Extend.
const (
	MinTTL = time.Minute
	MaxTTL = 168 * time.Hour
)

// maxExpiredHolders is how many expired holder records a lease keeps as
// history. Live holders are never dropped.
const maxExpiredHolders = 20

var (
	// ErrTTLOutOfRange is returned by Extend when ttl is outside MinTTL..MaxTTL.
	ErrTTLOutOfRange = errors.New("ttl out of range")
	// ErrCorrupt marks a state file that cannot be parsed.
	ErrCorrupt = errors.New("state file is corrupt")
)

// Holder is one claim on a lease.
type Holder struct {
	Name       string        `json:"name"`
	ExtendedAt time.Time     `json:"extended_at"`
	Expires    time.Time     `json:"expires"`
	TTL        time.Duration `json:"ttl"`
}

// Lease states. "" is the live state. StateStopping and StateRemoving are
// one-pass locks set while a stop or a removal runs, so extend is refused
// during either. StateStopped is set once the stop finished, with StoppedAt
// recorded. A new state value requires a new state-file version: the store
// refuses a version other than 1, so a build that does not know the new
// value refuses the whole file instead of mishandling it.
const (
	StateStopping = "stopping"
	StateStopped  = "stopped"
	StateRemoving = "removing"
)

// Lease is the state of one project. State is "" (live), "stopping" or
// "removing" (one-pass locks; extends are refused during either) or
// "stopped" (the stop finished, StoppedAt records when; the project is
// removed once its effective removal delay has passed). Holders survive a stop as expired
// history, so list and status can name the last holder.
type Lease struct {
	Project string `json:"project"`
	State   string `json:"state,omitempty"`
	// StoppedAt records when the stop finished (UTC, omitzero so a live
	// lease never carries it).
	StoppedAt time.Time `json:"stopped_at,omitzero"`
	// RemoveAfter is the removal delay in force when the project was
	// stopped: the dev-lease.remove-after label, or serve's setting at that
	// moment (RemoveAfterFrom says which). A pointer so an explicit 0 (never
	// remove) round-trips through JSON distinct from nil (no recorded
	// delay), which counts as serve-set with serve's current setting.
	// EffectiveRemovalDelay turns it into the delay in force now.
	RemoveAfter *time.Duration `json:"remove_after,omitempty"`
	// RemoveAfterFrom records where the stored delay came from: "label" or
	// "serve". A lease without an origin ("") is serve-set.
	RemoveAfterFrom string `json:"remove_after_from,omitempty"`
	// LabelRemoveAfter is the dev-lease.remove-after delay the last sweep
	// that saw containers for the project read from their labels. It is
	// recorded while the containers exist, on every sweep, so a later
	// compose down cannot erase the label before the stop: a stop with no
	// containers left falls back to this recorded value. Nil when no
	// container ever carried the label, or the last sweep saw containers
	// without one. Extend, release and revive leave it alone; the sweep
	// refreshes it every pass.
	LabelRemoveAfter *time.Duration `json:"label_remove_after,omitempty"`
	// Services are the com.docker.compose.service labels of the project's
	// containers, sorted and unique, recorded by the same sweep refresh
	// that records LabelRemoveAfter, so a later compose down cannot erase
	// them before the stop: the removal's image rule reads them to
	// recognise the project's unlabelled built images (Compose v2 builds
	// images without labels) once no container is left. Nil when no
	// container ever carried a service label, or the last sweep saw
	// containers without one. Extend, ExtendProject, release and revive
	// leave it alone; the sweep refreshes it every pass.
	Services []string `json:"services,omitempty"`
	Holders  []Holder `json:"holders"`
}

// RecordedDelay is the removal delay recorded at the stop, or the given
// fallback when none is recorded. EffectiveRemovalDelay is the one that
// decides a removal.
func (l Lease) RecordedDelay(fallback time.Duration) time.Duration {
	if l.RemoveAfter != nil {
		return *l.RemoveAfter
	}
	return fallback
}

// EffectiveRemovalDelay is the removal delay in force for a stopped lease
// now. The global off switch always wins: serve's current setting 0 or a
// stored 0 returns 0 (never remove). A label-set delay is the stored label
// value, unchanged by later serve settings. A serve-set delay is the longer
// of the recorded value and serve's current setting: never shorter than the
// delay recorded at the stop, and a longer current setting applies while it
// is in force. A lease without an origin (empty remove_after_from) is
// serve-set; a lease without a recorded delay (nil) is serve-set with
// serve's current setting.
func (l Lease) EffectiveRemovalDelay(serve time.Duration) time.Duration {
	if serve == 0 || (l.RemoveAfter != nil && *l.RemoveAfter == 0) {
		return 0
	}
	if l.RemoveAfterFrom == "label" && l.RemoveAfter != nil {
		return *l.RemoveAfter
	}
	stored := serve
	if l.RemoveAfter != nil {
		stored = *l.RemoveAfter
	}
	if stored > serve {
		return stored
	}
	return serve
}

// ExpiresAt returns the latest holder expiry, or the zero time when there are
// no holders.
func (l Lease) ExpiresAt() time.Time {
	var latest time.Time
	for _, h := range l.Holders {
		if h.Expires.After(latest) {
			latest = h.Expires
		}
	}
	return latest
}

// Expired reports whether the lease has no holders or every holder expired.
func (l Lease) Expired(now time.Time) bool {
	e := l.ExpiresAt()
	return e.IsZero() || !e.After(now)
}

// ValidateTTL reports whether ttl is inside MinTTL..MaxTTL.
func ValidateTTL(ttl time.Duration) error {
	if ttl < MinTTL || ttl > MaxTTL {
		return fmt.Errorf("%w: %v is outside %v..%v", ErrTTLOutOfRange, ttl, MinTTL, MaxTTL)
	}
	return nil
}

// The refusals of ExtendProject for a project the reaper is mid-flight on.
var (
	ErrBeingStopped = errors.New("the project is being stopped")
	ErrBeingRemoved = errors.New("the project is being removed")
)

// ExtendProject applies an extend to one project inside a Store.Update
// callback: a stopped mark is cleared (revived reports that, dev-lease
// never starts the containers) and holder's claim is extended by ttl.
// It refuses ErrBeingStopped and ErrBeingRemoved while the reaper is
// mid-flight on the project.
func ExtendProject(m map[string]Lease, project, holder string, ttl time.Duration, now time.Time) (revived bool, err error) {
	l := m[project]
	switch l.State {
	case StateStopping:
		return false, ErrBeingStopped
	case StateRemoving:
		return false, ErrBeingRemoved
	case StateStopped:
		l.State = ""
		l.StoppedAt = time.Time{}
		l.RemoveAfter = nil
		l.RemoveAfterFrom = ""
		revived = true
	}
	l.Project = project
	l, err = Extend(l, holder, ttl, now)
	if err != nil {
		return false, err
	}
	m[project] = l
	return revived, nil
}

// Extend returns l with holder's claim renewed: the holder's expiry becomes
// max(current expiry, now+ttl), so an extend never shortens a claim. All live
// holders are kept and at most maxExpiredHolders expired ones survive.
func Extend(l Lease, holder string, ttl time.Duration, now time.Time) (Lease, error) {
	if holder == "" {
		return l, errors.New("holder name must not be empty")
	}
	if err := ValidateTTL(ttl); err != nil {
		return l, err
	}
	out := l
	out.Holders = append([]Holder(nil), l.Holders...)
	target := now.Add(ttl)
	for i := range out.Holders {
		if out.Holders[i].Name == holder {
			out.Holders[i].ExtendedAt = now
			out.Holders[i].TTL = ttl
			if target.After(out.Holders[i].Expires) {
				out.Holders[i].Expires = target
			}
			out.Holders = trimHolders(out.Holders, now)
			return out, nil
		}
	}
	out.Holders = append(out.Holders, Holder{Name: holder, ExtendedAt: now, Expires: target, TTL: ttl})
	out.Holders = trimHolders(out.Holders, now)
	return out, nil
}

// Release ends holder's claim (or every claim when all is true) at now. The
// ended claim stays in the holder history as an expired entry.
func Release(l Lease, holder string, all bool, now time.Time) Lease {
	out := l
	out.Holders = append([]Holder(nil), l.Holders...)
	for i := range out.Holders {
		if all || out.Holders[i].Name == holder {
			out.Holders[i].Expires = now
		}
	}
	out.Holders = trimHolders(out.Holders, now)
	return out
}

// trimHolders keeps every live holder and, when more than maxExpiredHolders
// expired ones exist, the most recently expired ones.
func trimHolders(holders []Holder, now time.Time) []Holder {
	var live, expired []Holder
	for _, h := range holders {
		if h.Expires.After(now) {
			live = append(live, h)
		} else {
			expired = append(expired, h)
		}
	}
	if len(expired) <= maxExpiredHolders {
		return holders
	}
	sort.SliceStable(expired, func(i, j int) bool {
		return expired[i].Expires.After(expired[j].Expires)
	})
	out := make([]Holder, 0, len(live)+maxExpiredHolders)
	out = append(out, live...)
	out = append(out, expired[:maxExpiredHolders]...)
	return out
}
