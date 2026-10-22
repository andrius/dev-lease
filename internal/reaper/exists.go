package reaper

import (
	"context"
)

// Exists reports whether project has anything dev-lease manages: a lease
// record, or something on the daemon that is not dev-lease.keep. The
// dashboard's keep and release ask it before acting, so an action on a
// name with nothing behind it is a clean 404 instead of a silently
// created lease.
func (r *Runner) Exists(ctx context.Context, project string) (bool, error) {
	leases, err := r.Store.Read()
	if err != nil {
		return false, err
	}
	if _, ok := leases[project]; ok {
		return true, nil
	}
	empty, err := projectEmpty(ctx, r.Cli, project, nil)
	if err != nil {
		return false, err
	}
	return !empty, nil
}
