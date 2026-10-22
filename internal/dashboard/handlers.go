package dashboard

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/andrius/dev-lease/internal/lease"
	"github.com/andrius/dev-lease/internal/reaper"
	"github.com/andrius/dev-lease/internal/view"
)

// page is the data every template reads. Mode picks the body template;
// only the fields the mode uses are filled.
type page struct {
	CSRF        string
	Now         time.Time
	Mode        string // "stacks", "stack", "login" or "error"
	FragmentURL string
	Flash       string

	// stacks mode
	Stacks   []stackRow
	Summary  string
	Patterns string

	// stack mode
	D        *view.Detail
	Page     *stackPage
	Services []string
	// LogLines is the log Lines select's selected option; LogLineOptions
	// are the options it offers (20, 50, 100, 200, plus the URL's own
	// value when it is none of those).
	LogLines       int
	LogLineOptions []int

	// login and error modes
	Status     int
	StatusText string
	Message    string
}

// stackRow is one row of the all-stacks page, pre-rendered so the
// template stays dumb.
type stackRow struct {
	Name, Path   string
	State        string
	Badge        string // ok, warn, bad or new
	Next         string
	NextTimeAttr string // RFC3339 UTC, "" for never removed
	NextAtText   string // the mockup's clock under the next event
	NextBad      bool
	Containers   string
	Disk         string
	HeldBy       string
}

// stackPage is the stack page's computed extras.
type stackPage struct {
	// LeaseLeft is the lease card's big text: "1h38m left",
	// "stopped 6d22h ago", "stopping now", "removing now" or
	// "no lease yet".
	LeaseLeft  string
	StoppedAgo time.Duration // for a stopped stack
	// RemoveAfter is the effective removal delay shown in the lease card
	// and the stop dialog for a live stack.
	RemoveAfter time.Duration
	// NeverRemove reports an effective delay of 0: the dialogs say
	// "never removed" and the remove form sends override=1.
	NeverRemove bool
	// Holders with the remaining time and the UTC instant each.
	Holders []holderRow
	// Confirm is "stop" or "remove" when the dialog renders as a page.
	Confirm string
	// DashHolds reports a live claim of the dashboard holder.
	DashHolds bool
	// StopAfter is the stop dialog's removal delay text ("7d").
	StopAfter time.Duration
	// KeptRunning is the stop dialog's list of kept containers that
	// keep running.
	KeptRunning []string
	// LeftInPlace is the remove dialog's list of what stays: the plan's
	// kept objects and the images a removal does not remove (pulled
	// images included).
	LeftInPlace []string
}

type holderRow struct {
	Name      string
	Remaining time.Duration
	Until     string // HH:MM UTC, rendered with the UTC suffix
}

// logLineOptions are the log Lines select's default options. A lines
// value outside them becomes its own selected option, so the URL and
// the select always agree on the exact value.
var logLineOptions = []int{20, 50, 100, 200}

// lineOptionsFor returns the select's options for one page: the
// defaults plus the requested lines value when it is outside them.
func lineOptionsFor(lines int) []int {
	if slices.Contains(logLineOptions, lines) {
		return logLineOptions
	}
	opts := append(slices.Clone(logLineOptions), lines)
	slices.Sort(opts)
	return opts
}

// --- pages ---

func (s *server) stacks(w http.ResponseWriter, r *http.Request) {
	data, err := s.stacksData(r)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, "dev-lease could not read the daemon in time; try again: "+err.Error())
		return
	}
	s.render(w, r, "page-stacks", data)
}

func (s *server) fragmentStacks(w http.ResponseWriter, r *http.Request) {
	data, err := s.stacksData(r)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, "dev-lease could not read the daemon in time; try again: "+err.Error())
		return
	}
	s.render(w, r, "stacks-frag", data)
}

func (s *server) stacksData(r *http.Request) (page, error) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(pageTimeout.Load()))
	defer cancel()
	now := s.now().UTC()
	stacks, err := view.List(ctx, s.runner.Cli, s.runner.Store, s.cfg(), now)
	if err != nil {
		return page{}, err
	}
	data := page{
		CSRF:        s.csrfAction(r),
		Now:         now,
		Mode:        "stacks",
		FragmentURL: "/fragments/stacks",
	}
	data.Stacks = make([]stackRow, 0, len(stacks))
	var live, stopped, removing int
	var disk int64
	unknown := false
	for _, st := range stacks {
		data.Stacks = append(data.Stacks, rowFrom(st, now))
		switch st.State {
		case view.StateLive:
			live++
		case view.StateStopped:
			stopped++
		case view.StateRemoving:
			removing++
		}
		if st.DiskBytes < 0 {
			unknown = true
		} else {
			disk += st.DiskBytes
		}
	}
	parts := []string{fmt.Sprintf("%d stack", len(stacks))}
	if len(stacks) != 1 {
		parts[0] += "s"
	}
	parts = append(parts, fmt.Sprintf("%d live", live), fmt.Sprintf("%d stopped", stopped), fmt.Sprintf("%d removing", removing))
	if !unknown {
		parts = append(parts, view.FmtSize(disk)+" on disk")
	}
	data.Summary = strings.Join(parts, " · ")
	data.Patterns = strings.Join(s.cfg().Patterns, ", ")
	data.Flash = s.flash(r, data)
	return data, nil
}

// rowFrom pre-renders one all-stacks row from a view.Stack.
func rowFrom(s view.Stack, now time.Time) stackRow {
	r := stackRow{
		Name:  s.Name,
		Path:  s.Path,
		State: s.State,
		Next:  s.Next,
		Disk:  view.FmtSize(s.DiskBytes),
	}
	if !s.NextAt.IsZero() {
		r.NextTimeAttr = s.NextAt.UTC().Format(time.RFC3339)
		r.NextAtText = clockText(s.NextAt, now)
	} else if s.State == view.StateRemoving && !s.StoppedAt.IsZero() {
		// The mockup's removing row carries "since 08:52 UTC".
		r.NextAtText = "since " + s.StoppedAt.UTC().Format("15:04") + " UTC"
	}
	r.Badge = badgeFor(s.State)
	r.NextBad = s.Soon && (s.State == view.StateStopped || s.State == view.StateRemoving)
	switch {
	case s.Running > 0 && s.Stopped > 0:
		r.Containers = fmt.Sprintf("%d running · %d stopped", s.Running, s.Stopped)
	case s.Running > 0:
		r.Containers = fmt.Sprintf("%d running", s.Running)
	case s.Stopped > 0:
		r.Containers = fmt.Sprintf("%d stopped", s.Stopped)
	default:
		r.Containers = "-"
	}
	if s.Running > 0 && s.Up > 0 {
		r.Containers += " · up " + view.FmtDur(s.Up)
	}
	if len(s.Holders) == 0 {
		r.HeldBy = "-"
	} else {
		names := make([]string, 0, len(s.Holders))
		for _, h := range s.Holders {
			names = append(names, h.Name)
		}
		r.HeldBy = strings.Join(names, ", ")
	}
	return r
}

// badgeFor maps a view state onto the badge class the mockup uses.
func badgeFor(state string) string {
	switch state {
	case view.StateLive:
		return "ok"
	case view.StateStopping, view.StateStopped:
		return "warn"
	case view.StateRemoving:
		return "bad"
	case view.StateNew:
		return "new"
	}
	return "new"
}

// clockText is the mockup's UTC clock under a next event: "14:05 UTC"
// on the event's day, "Oct 26, 08:10 UTC" otherwise.
func clockText(t, now time.Time) string {
	tu := t.UTC()
	if tu.Year() == now.Year() && tu.YearDay() == now.YearDay() {
		return tu.Format("15:04") + " UTC"
	}
	return tu.Format("Jan 2, 15:04") + " UTC"
}

func (s *server) stack(w http.ResponseWriter, r *http.Request) {
	data, status, err := s.stackData(r, r.PathValue("name"))
	if err != nil {
		s.errorPage(w, r, status, err.Error())
		return
	}
	s.render(w, r, "page-stack", data)
}

func (s *server) fragmentStack(w http.ResponseWriter, r *http.Request) {
	data, status, err := s.stackData(r, r.PathValue("name"))
	if err != nil {
		s.errorPage(w, r, status, err.Error())
		return
	}
	s.render(w, r, "stack-frag", data)
}

func (s *server) stackData(r *http.Request, name string) (page, int, error) {
	q := r.URL.Query()
	logService := q.Get("logs")
	lines := 50
	if lv := q.Get("lines"); lv != "" {
		n, err := strconv.Atoi(lv)
		if err != nil || n < 1 || n > 200 {
			return page{}, http.StatusBadRequest, errors.New("lines must be a number from 1 to 200")
		}
		lines = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(pageTimeout.Load()))
	defer cancel()
	now := s.now().UTC()
	d, err := view.Details(ctx, s.runner.Cli, s.runner.Store, s.cfg(), name, logService, lines, now)
	if err != nil {
		switch {
		case errors.Is(err, view.ErrNotManaged):
			return page{}, http.StatusNotFound, errors.New("there is no managed stack with that name")
		case errors.Is(err, view.ErrNoStack):
			return page{}, http.StatusNotFound, errors.New("there is no managed stack with that name")
		default:
			return page{}, http.StatusInternalServerError, errors.New("dev-lease could not read the daemon in time; try again: " + err.Error())
		}
	}
	p := stackPage{RemoveAfter: d.RemoveAfter, NeverRemove: d.NeverRemove}
	switch d.State {
	case view.StateLive:
		p.LeaseLeft = view.FmtDur(d.NextAt.Sub(now)) + " left"
	case view.StateStopped:
		p.StoppedAgo = now.Sub(d.StoppedAt)
		p.LeaseLeft = "stopped " + view.FmtDur(p.StoppedAgo) + " ago"
	case view.StateStopping:
		p.LeaseLeft = "stopping now"
	case view.StateRemoving:
		p.LeaseLeft = "removing now"
	default:
		p.LeaseLeft = "no lease yet"
	}
	for _, h := range d.Holders {
		p.Holders = append(p.Holders, holderRow{
			Name:      h.Name,
			Remaining: h.Until.Sub(now),
			Until:     h.Until.UTC().Format("15:04"),
		})
		if h.Name == "dashboard" {
			p.DashHolds = true
		}
	}
	// The stop dialog's removal delay: for a stopped stack the view's
	// Next already says it ("removed in 1h40m"); for anything else it is
	// the effective delay after a stop, never a "<1m" for never remove.
	if d.State == view.StateStopped && !d.NeverRemove {
		p.StopAfter = d.NextAt.Sub(now)
	} else {
		p.StopAfter = d.RemoveAfter
	}
	for _, c := range d.Containers {
		if c.Kept && c.State == "running" {
			p.KeptRunning = append(p.KeptRunning, c.Name)
		}
	}
	// What stays on a removal: the plan's kept objects, plus the images
	// that are not removed (the view's kept images include the plan's
	// kept images, so one pass through suffices).
	p.LeftInPlace = append([]string(nil), d.Plan.Kept...)
	for _, k := range d.KeptImages {
		if !slices.Contains(p.LeftInPlace, k) {
			p.LeftInPlace = append(p.LeftInPlace, k)
		}
	}
	seen := map[string]bool{}
	frag := "/fragments/stacks/" + url.PathEscape(name)
	fq := url.Values{}
	if logService != "" {
		fq.Set("logs", logService)
	}
	if lv := q.Get("lines"); lv != "" {
		fq.Set("lines", strconv.Itoa(lines))
	}
	if len(fq) > 0 {
		frag += "?" + fq.Encode()
	}
	data := page{
		CSRF:           s.csrfAction(r),
		Now:            now,
		Mode:           "stack",
		FragmentURL:    frag,
		D:              &d,
		Page:           &p,
		LogLines:       lines,
		LogLineOptions: lineOptionsFor(lines),
	}
	for _, c := range d.Containers {
		if c.Service != "" && !seen[c.Service] {
			seen[c.Service] = true
			data.Services = append(data.Services, c.Service)
		}
	}
	sort.Strings(data.Services)
	if c := q.Get("confirm"); c == "stop" || c == "remove" {
		data.Page.Confirm = c
	}
	data.Flash = s.flash(r, data)
	return data, 0, nil
}

// favicon serves the embedded icon at the conventional /favicon.ico.
func (s *server) favicon(w http.ResponseWriter, r *http.Request) {
	b, err := assets.ReadFile("static/favicon.svg")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(b)
}

func (s *server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "page-login", page{CSRF: s.csrfLogin, Now: s.now().UTC(), Mode: "login"})
}

// loginDelay is how long a wrong login waits before the 401: guessing
// tokens must be slow. Tests shrink it.
var loginDelay atomic.Int64

func init() {
	loginDelay.Store(int64(time.Second))
}

// loginPost checks the token form field against the configured token:
// right, the dl_session cookie is set and the browser lands on /; wrong,
// a delayed 401. The cookie is Secure over TLS.
func (s *server) loginPost(w http.ResponseWriter, r *http.Request) {
	got := r.PostFormValue("token")
	if subtle.ConstantTimeCompare([]byte(got), s.token) != 1 {
		time.Sleep(time.Duration(loginDelay.Load()))
		s.errorPage(w, r, http.StatusUnauthorized, "wrong token")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "dl_session", Value: s.sessionCookie(), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// errorPage renders the error template with the given status.
func (s *server) errorPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	data := page{
		CSRF: s.csrf, Now: s.now().UTC(), Mode: "error",
		Status: status, StatusText: http.StatusText(status), Message: message,
	}
	s.render(w, r, "page-error", data)
}

// render executes one template set into w with the status it carries.
func (s *server) render(w http.ResponseWriter, r *http.Request, name string, data page) {
	status := http.StatusOK
	if data.Mode == "error" {
		status = data.Status
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		// The headers are out; a template bug shows as a cut page in the
		// serve log via the middleware, never as a hung request.
		return
	}
}

// flash maps the fixed flash codes of a redirect's query to texts. Only
// the fixed codes are reflected; anything else is ignored, never echoed.
func (s *server) flash(r *http.Request, data page) string {
	q := r.URL.Query()
	if len(q) == 0 {
		return ""
	}
	name := ""
	if data.D != nil {
		name = data.D.Name
	}
	switch {
	case q.Get("done") == "removed":
		if name == "" {
			// The removal lands on the all-stacks page, whose data has no
			// Detail: the stack's name rides in the query instead, and it
			// is shown only when the serve config manages it. Anything
			// else stays out of the page.
			if qn := q.Get("stack"); reaper.Managed(qn, s.cfg()) {
				name = qn
			}
		}
		if name == "" {
			return "The stack was removed."
		}
		return "Removed " + name + "."
	case q.Get("done") == "stopped":
		return "Stopped " + name + "."
	case q.Get("done") == "released":
		return "Released your claim."
	case q.Get("done") == "kept":
		ttl, ok := ttlByCode(q.Get("ttl"))
		if !ok {
			return ""
		}
		// Keeping a stopped stack revives its lease but dev-lease never
		// starts containers: the keep action records what the lease was
		// before it ran and the flash says so with the start command.
		if q.Get("was") == "stopped" && data.D != nil {
			return name + "'s lease is live again, but the stack is still stopped; start it with: " + data.D.StartCommand
		}
		return "Kept " + name + " for at least " + ttl + "."
	case q.Get("err") == "busy":
		return name + " is being stopped or removed right now; try again in a moment."
	case q.Get("err") == "nothing_to_release":
		return "There is nothing to release: the dashboard holds no claim."
	case q.Get("err") == "not_removed":
		return name + " was not removed: a container is running again or a delete failed; see serve's log."
	case q.Get("err") == "not_stopped":
		return "Some containers of " + name + " could not be stopped; see serve's log."
	}
	return ""
}

// ttlByCode maps the fixed ttl codes of the keep form to their texts.
func ttlByCode(code string) (string, bool) {
	switch code {
	case "2h":
		return "2 hours", true
	case "8h":
		return "8 hours", true
	case "1d":
		return "1 day", true
	case "7d":
		return "7 days", true
	}
	return "", false
}

// --- actions ---

// ctx is the context an action runs on: serve's own context, so a closed
// browser tab never aborts a half-done stop or removal.
func (s *server) ctx() context.Context {
	if s.base != nil {
		return s.base
	}
	return context.Background()
}

// extendDeadline pushes the response write deadline out before the
// runner is called: a removal can outlast the server's WriteTimeout.
func (s *server) extendDeadline(w http.ResponseWriter) {
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(actionWriteDeadline))
	}
}

func (s *server) keep(w http.ResponseWriter, r *http.Request) {
	s.extendDeadline(w)
	name := r.PathValue("name")
	ttl, ok := keepTTL(r.PostFormValue("ttl"))
	if !ok {
		s.errorPage(w, r, http.StatusBadRequest, "keep needs ttl 2h, 8h, 1d or 7d")
		return
	}
	if !reaper.Managed(name, s.cfg()) {
		s.errorPage(w, r, http.StatusNotFound, "there is no managed stack with that name")
		return
	}
	exists, err := s.runner.Exists(s.ctx(), name)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, "keep failed: "+err.Error())
		return
	}
	if !exists {
		s.errorPage(w, r, http.StatusNotFound, "there is no stack named "+name)
		return
	}
	wasStopped := false
	err = s.runner.Store.Update(func(m map[string]lease.Lease) error {
		if l, ok := m[name]; ok && l.State == lease.StateStopped {
			wasStopped = true
		}
		_, err := lease.ExtendProject(m, name, "dashboard", ttl, s.now().UTC())
		return err
	})
	switch {
	case errors.Is(err, lease.ErrBeingStopped), errors.Is(err, lease.ErrBeingRemoved):
		s.errorPage(w, r, http.StatusConflict, name+" is being stopped or removed right now; try again in a moment")
		return
	case err != nil:
		s.errorPage(w, r, http.StatusInternalServerError, "keep failed: "+err.Error())
		return
	}
	loc := "/stacks/" + url.PathEscape(name) + "?done=kept&ttl=" + ttlCode(ttl)
	if wasStopped {
		loc += "&was=stopped"
	}
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

// release ends the dashboard holder's claim only.
func (s *server) release(w http.ResponseWriter, r *http.Request) {
	s.extendDeadline(w)
	name := r.PathValue("name")
	if !reaper.Managed(name, s.cfg()) {
		s.errorPage(w, r, http.StatusNotFound, "there is no managed stack with that name")
		return
	}
	exists, err := s.runner.Exists(s.ctx(), name)
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, "release failed: "+err.Error())
		return
	}
	if !exists {
		s.errorPage(w, r, http.StatusNotFound, "there is no stack named "+name)
		return
	}
	nothing := false
	now := s.now().UTC()
	err = s.runner.Store.Update(func(m map[string]lease.Lease) error {
		l, ok := m[name]
		if !ok {
			// No lease at all: there is no dashboard claim to release.
			nothing = true
			return nil
		}
		// A release needs a live dashboard claim: without one nothing is
		// released and the flash says so.
		has := false
		for _, h := range l.Holders {
			if h.Name == "dashboard" && h.Expires.After(now) {
				has = true
				break
			}
		}
		if !has {
			nothing = true
			return nil
		}
		m[name] = lease.Release(l, "dashboard", false, now)
		return nil
	})
	if err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, "release failed: "+err.Error())
		return
	}
	if nothing {
		http.Redirect(w, r, "/stacks/"+url.PathEscape(name)+"?err=nothing_to_release", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/stacks/"+url.PathEscape(name)+"?done=released", http.StatusSeeOther)
}

func (s *server) stop(w http.ResponseWriter, r *http.Request) {
	s.extendDeadline(w)
	name := r.PathValue("name")
	rep, err := s.runner.StopNow(s.ctx(), name, "dashboard")
	switch {
	case errors.Is(err, reaper.ErrNotManaged), errors.Is(err, reaper.ErrNoStack):
		s.errorPage(w, r, http.StatusNotFound, "there is no managed stack with that name")
	case errors.Is(err, reaper.ErrBusy):
		s.errorPage(w, r, http.StatusConflict, name+" is being stopped or removed right now; try again in a moment")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		s.errorPage(w, r, http.StatusServiceUnavailable, "dev-lease is shutting down")
	case err != nil:
		s.errorPage(w, r, http.StatusInternalServerError, "stop failed: "+err.Error())
	case len(rep.Failed) > 0 || len(rep.Skipped) > 0:
		// Some containers could not be stopped; the flash says which
		// stack and points at the serve log.
		http.Redirect(w, r, "/stacks/"+url.PathEscape(name)+"?err=not_stopped", http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/stacks/"+url.PathEscape(name)+"?done=stopped", http.StatusSeeOther)
	}
}

func (s *server) remove(w http.ResponseWriter, r *http.Request) {
	s.extendDeadline(w)
	name := r.PathValue("name")
	if !reaper.Managed(name, s.cfg()) {
		s.errorPage(w, r, http.StatusNotFound, "there is no managed stack with that name")
		return
	}
	if r.PostFormValue("confirm") != name {
		s.errorPage(w, r, http.StatusBadRequest, "the typed name does not match; type "+name+" to confirm")
		return
	}
	rep, err := s.runner.RemoveNow(s.ctx(), name, reaper.RemoveOptions{
		Via: "dashboard", AllowNeverRemove: r.PostFormValue("override") == "1",
	})
	switch {
	case errors.Is(err, reaper.ErrNotManaged), errors.Is(err, reaper.ErrNoStack):
		s.errorPage(w, r, http.StatusNotFound, "there is no managed stack with that name")
	case errors.Is(err, reaper.ErrBusy):
		s.errorPage(w, r, http.StatusConflict, name+" is being stopped or removed right now; try again in a moment")
	case errors.Is(err, reaper.ErrNeverRemove):
		s.errorPage(w, r, http.StatusConflict, name+" is marked never remove; the dialog must send override=1")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		s.errorPage(w, r, http.StatusServiceUnavailable, "dev-lease is shutting down")
	case err != nil:
		s.errorPage(w, r, http.StatusInternalServerError, "remove failed: "+err.Error())
	case !reportRemoved(rep, name):
		s.errorPage(w, r, http.StatusConflict, name+" was not removed: a container is running again or a delete failed; see serve's log")
	default:
		http.Redirect(w, r, "/?done=removed&stack="+url.PathEscape(name), http.StatusSeeOther)
	}
}

// reportRemoved reports whether rep's sweep removed the project.
func reportRemoved(rep reaper.Report, project string) bool {
	for _, p := range rep.Removed {
		if p == project {
			return true
		}
	}
	return false
}

// keepTTL maps the form values to durations; anything else is refused.
func keepTTL(v string) (time.Duration, bool) {
	switch v {
	case "2h":
		return 2 * time.Hour, true
	case "8h":
		return 8 * time.Hour, true
	case "1d":
		return 24 * time.Hour, true
	case "7d":
		return 7 * 24 * time.Hour, true
	}
	return 0, false
}

func ttlCode(d time.Duration) string {
	switch d {
	case 2 * time.Hour:
		return "2h"
	case 8 * time.Hour:
		return "8h"
	case 24 * time.Hour:
		return "1d"
	case 7 * 24 * time.Hour:
		return "7d"
	}
	return ""
}
