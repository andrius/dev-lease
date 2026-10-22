// Package docker is a minimal Docker Engine API client restricted to exactly
// the calls dev-lease needs: read project labels, inspect its own container,
// stop and remove containers, and remove networks, volumes and images.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotAllowed marks a request the allowlist refused before any socket I/O.
var ErrNotAllowed = errors.New("request not allowed by the dev-lease allowlist")

const (
	// ProjectLabel is the compose label naming the project an object
	// belongs to.
	ProjectLabel = "com.docker.compose.project"
	// RoleLabel marks the dev-lease containers: a serve container carries
	// the value "server".
	RoleLabel = "dev-lease.role"
	// ServiceLabel is the compose label naming the service an image was
	// built for (images) or the container runs (containers).
	ServiceLabel = "com.docker.compose.service"
	// KeepLabel is the dev-lease policy label: on a container or volume
	// with value "true" the object is never stopped and never removed.
	// Any other value is ignored and logged.
	KeepLabel = "dev-lease.keep"
	// RemoveAfterLabel overrides serve's --remove-after for the project:
	// a Go duration, whole days ("7d") or "0" for never. Read from the
	// project's containers when the project is stopped.
	RemoveAfterLabel = "dev-lease.remove-after"
)

var apiVersionRe = regexp.MustCompile(`^(\d+)\.(\d+)`)

// Object is a labelled container, network, volume or image.
type Object struct {
	ID      string
	Name    string
	ImageID string
	// ImageRef is the image reference the container was created from (the
	// listing's Image field), for example postgres:16-alpine. Empty when
	// the daemon did not report one.
	ImageRef string
	Labels   map[string]string
	// RepoTags are the repository tags of an image (empty when untagged).
	RepoTags []string
	// Untagged reports whether the image has no repository tag left
	// (compose up --build orphans the old tag).
	Untagged bool
	// Running reports whether a container is running (from the container
	// listing's State field).
	Running bool
	// Created is when the container was created (the listing's Created
	// field, a Unix timestamp in seconds). Zero for non-containers.
	Created time.Time
	// Status is the human-readable status string of the listing (for
	// example "Up 46 minutes"); empty for non-containers.
	Status string
	// Networks are the names of the networks the container is attached to
	// (NetworkSettings.Networks keys).
	Networks []string
	// MountNames are the named volumes the container mounts (Mounts
	// entries with Type volume).
	MountNames []string
}

// LogLine is one line of a container's log, parsed from the daemon's
// timestamped multiplexed or raw (TTY) stream.
type LogLine struct {
	Time time.Time
	// Stream is "stdout" or "stderr".
	Stream string
	Text   string
}

// Kept reports whether the object carries dev-lease.keep=true.
func Kept(o Object) bool {
	return o.Labels[KeepLabel] == "true"
}

// StatusError is a non-2xx answer from the daemon.
type StatusError struct {
	Method string
	Path   string
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("docker: %s %s: status %d", e.Method, e.Path, e.Status)
}

// Client talks to the Engine API over a Unix socket.
type Client struct {
	socketPath string
	http       *http.Client

	mu         sync.Mutex
	apiVersion string
	ownID      string
	// stopHTTP and stopMargin are the stop request's own budget.
	stopHTTP   *http.Client
	stopMargin time.Duration

	// sizeMu guards the volume size cache: one /system/df answer per
	// minute at most, whatever the number of concurrent readers.
	sizeMu  sync.Mutex
	sizes   map[string]int64
	sizesAt time.Time
}

// requestTimeout caps one request. The longest legitimate request is a
// container stop (--stop-timeout); anything past two minutes is a daemon
// that is stuck, not one that is working.
const requestTimeout = 2 * time.Minute

// stopMargin is added to the stop timeout to form the stop request's own
// budget, which can exceed requestTimeout when the operator raises
// --stop-timeout.
const stopMargin = 30 * time.Second

// New returns a client for the given Docker socket. No socket I/O happens
// until the first call.
func New(socketPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		socketPath: socketPath,
		http: &http.Client{
			Timeout:   requestTimeout,
			Transport: transport,
			// The allowlist checked the request; a redirect would point at a
			// path that was never checked.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("redirect refused")
			},
		},
		// Stop requests carry their own budget (stop timeout + margin),
		// so this client has no fixed timeout.
		stopHTTP:   &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }},
		stopMargin: stopMargin,
	}
}

// PinOwnID pins the id of dev-lease's own container: from then on the client
// refuses to inspect any other container.
func (c *Client) PinOwnID(ownID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ownID = ownID
}

// IsUnreachable reports whether err is a transport failure (daemon down or
// a request that timed out), as opposed to an answer from the daemon or a
// local permission problem.
func IsUnreachable(err error) bool {
	if IsPermission(err) {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// IsPermission reports whether err is a definitive permission denial
// (EACCES, EPERM) from the socket transport: retrying will not help, unlike
// a daemon that is down. Only net.OpError counts, so a permission error on
// the state file or anywhere else is not mislabelled as a socket problem.
func IsPermission(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && os.IsPermission(opErr.Err)
}

// Projects returns the set of Compose project names found on any object of
// the daemon by the com.docker.compose.project label key. Every list call
// carries a key-only label filter; the container listing adds all=1 so a
// stopped leftover keeps its project present. A kept object (dev-lease.keep
// on the object itself or on the container using it) does not make its
// project present.
func (c *Client) Projects(ctx context.Context) (map[string]bool, error) {
	projects := map[string]bool{}
	keptNets := map[string]bool{}
	keptVols := map[string]bool{}
	keptImgs := map[string]bool{}

	// Containers first: a kept container itself does not count, and what
	// it uses (networks from NetworkSettings, volumes from Mounts, its
	// image) does not either.
	var containers []struct {
		Id              string            `json:"Id"`
		Names           []string          `json:"Names"`
		ImageID         string            `json:"ImageID"`
		Labels          map[string]string `json:"Labels"`
		NetworkSettings struct {
			Networks map[string]json.RawMessage `json:"Networks"`
		} `json:"NetworkSettings"`
		Mounts []struct {
			Type string `json:"Type"`
			Name string `json:"Name"`
		} `json:"Mounts"`
	}
	if err := c.getJSON(ctx, "/containers/json", map[string][]string{"label": {ProjectLabel}}, url.Values{"all": {"1"}}, &containers); err != nil {
		return nil, err
	}
	for _, it := range containers {
		if Kept(Object{Labels: it.Labels}) {
			if it.ImageID != "" {
				keptImgs[it.ImageID] = true
			}
			for n := range it.NetworkSettings.Networks {
				keptNets[n] = true
			}
			for _, m := range it.Mounts {
				if m.Type == "volume" && m.Name != "" {
					keptVols[m.Name] = true
				}
			}
			continue
		}
		if p := it.Labels[ProjectLabel]; p != "" {
			projects[p] = true
		}
	}

	var networks []struct {
		Id     string            `json:"Id"`
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
	}
	if err := c.listAll(ctx, "/networks", &networks); err != nil {
		return nil, err
	}
	for _, it := range networks {
		if keptNets[it.Name] {
			continue
		}
		if p := it.Labels[ProjectLabel]; p != "" {
			projects[p] = true
		}
	}

	var volResp struct {
		Volumes []struct {
			Name   string            `json:"Name"`
			Labels map[string]string `json:"Labels"`
		} `json:"Volumes"`
	}
	if err := c.listAll(ctx, "/volumes", &volResp); err != nil {
		return nil, err
	}
	for _, v := range volResp.Volumes {
		o := Object{ID: v.Name, Name: v.Name, Labels: v.Labels}
		if Kept(o) || keptVols[v.Name] {
			continue
		}
		if p := v.Labels[ProjectLabel]; p != "" {
			projects[p] = true
		}
	}

	var images []struct {
		Id       string            `json:"Id"`
		RepoTags []string          `json:"RepoTags"`
		Labels   map[string]string `json:"Labels"`
	}
	if err := c.listAll(ctx, "/images/json", &images); err != nil {
		return nil, err
	}
	for _, it := range images {
		if keptImgs[it.Id] {
			continue
		}
		p := it.Labels[ProjectLabel]
		if p == "" {
			continue
		}
		// Presence uses the same rule as removal (ImageTagMatches): a tag
		// of the <project>-<service> shape for the image's own service
		// label. An image built FROM a project image inherits the project
		// label but carries its own tag: it does not count. An untagged
		// leftover never counts on its own: it is removable only when the
		// removal collects the id of an image the containers ran from.
		// An unlabelled image never makes a project present, whatever
		// its tags: there is no label to find the project by.
		if ImageTagMatches(p, Object{ID: it.Id, Labels: it.Labels, RepoTags: it.RepoTags}) {
			projects[p] = true
		}
	}
	return projects, nil
}

// ImageTagMatches reports whether the image carries a repository tag of the
// shape <project>-<service>:<tag> where <service> is the image's own
// service label: the name compose gives an image built for one service of
// the project. The repository part is the whole of the tag before its tag
// colon (the last colon, when that comes after the last slash), registry
// host and path included, so ghcr.io/andrius/myapp-web:dev never
// matches a myapp project's web service. Images that only inherited
// the labels (built FROM a project image) never match, whatever their
// tags.
func ImageTagMatches(project string, i Object) bool {
	service := i.Labels[ServiceLabel]
	if service == "" {
		return false
	}
	return TagMatchesService(project, service, i.RepoTags)
}

// TagMatchesService reports whether one of the image's repository tags
// has the repository part exactly <project>-<service>: the tag name
// compose gives an image built for one service of the project. The
// repository part is the whole of the tag before its tag colon (see
// repository), so a registry host and path never match.
func TagMatchesService(project, service string, tags []string) bool {
	for _, tag := range tags {
		if repo, ok := repository(tag); ok && repo == project+"-"+service {
			return true
		}
	}
	return false
}

// TagService returns the service among services whose <project>-<service>
// is exactly the repository of tag, the way TagMatchesService matches.
func TagService(project string, services []string, tag string) (string, bool) {
	repo, ok := repository(tag)
	if !ok {
		return "", false
	}
	for _, s := range services {
		if repo == project+"-"+s {
			return s, true
		}
	}
	return "", false
}

// repository returns the repository part of a repository tag: everything
// before the last ':' when that colon comes after the last '/' (a tag
// colon), registry host and path included; otherwise the tag has no tag
// colon and matches nothing. "myapp-web:latest" gives "myapp-web";
// "ghcr.io/andrius/myapp-web:dev" gives
// "ghcr.io/andrius/myapp-web"; "myapp-web:5000/team/tool:1" gives
// "myapp-web:5000/team/tool", never the <project>-<service> shape.
func repository(tag string) (string, bool) {
	i := strings.LastIndex(tag, ":")
	if i <= strings.LastIndex(tag, "/") {
		return "", false
	}
	return tag[:i], true
}

// listAll is a list call filtered to objects carrying the project label key.
func (c *Client) listAll(ctx context.Context, path string, out any) error {
	return c.getJSON(ctx, path, map[string][]string{"label": {ProjectLabel}}, nil, out)
}

// Containers lists the containers of one project (all=1, so stopped
// containers are included).
func (c *Client) Containers(ctx context.Context, project string) ([]Object, error) {
	var filter map[string][]string
	if project != "" {
		filter = map[string][]string{"label": {ProjectLabel + "=" + project}}
	}
	return c.containersJSON(ctx, filter, true)
}

// RoleServers lists the running containers that carry the
// dev-lease.role=server label (stopped ones are not included).
func (c *Client) RoleServers(ctx context.Context) ([]Object, error) {
	return c.containersJSON(ctx, map[string][]string{"label": {RoleLabel + "=server"}}, false)
}

// containersJSON lists containers with one label filter; all includes the
// stopped ones.
func (c *Client) containersJSON(ctx context.Context, filter map[string][]string, all bool) ([]Object, error) {
	var list []struct {
		Id              string            `json:"Id"`
		Names           []string          `json:"Names"`
		ImageID         string            `json:"ImageID"`
		Image           string            `json:"Image"`
		Labels          map[string]string `json:"Labels"`
		State           string            `json:"State"`
		Created         int64             `json:"Created"`
		Status          string            `json:"Status"`
		NetworkSettings struct {
			Networks map[string]json.RawMessage `json:"Networks"`
		} `json:"NetworkSettings"`
		Mounts []struct {
			Type string `json:"Type"`
			Name string `json:"Name"`
		} `json:"Mounts"`
	}
	qv := url.Values{}
	if all {
		qv.Set("all", "1")
	}
	if err := c.getJSON(ctx, "/containers/json", filter, qv, &list); err != nil {
		return nil, err
	}
	out := make([]Object, 0, len(list))
	for _, it := range list {
		name := ""
		if len(it.Names) > 0 {
			name = strings.TrimPrefix(it.Names[0], "/")
		}
		nets := make([]string, 0, len(it.NetworkSettings.Networks))
		for n := range it.NetworkSettings.Networks {
			nets = append(nets, n)
		}
		sort.Strings(nets)
		var mounts []string
		for _, m := range it.Mounts {
			if m.Type == "volume" && m.Name != "" {
				mounts = append(mounts, m.Name)
			}
		}
		out = append(out, Object{ID: it.Id, Name: name, ImageID: it.ImageID, ImageRef: it.Image, Labels: it.Labels,
			Running: it.State == "running", Created: time.Unix(it.Created, 0).UTC(), Status: it.Status,
			Networks: nets, MountNames: mounts})
	}
	return out, nil
}

// Networks lists the networks of one project (all when project is empty).
func (c *Client) Networks(ctx context.Context, project string) ([]Object, error) {
	var list []struct {
		Id     string            `json:"Id"`
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
	}
	if err := c.getJSON(ctx, "/networks", projectFilter(project), nil, &list); err != nil {
		return nil, err
	}
	out := make([]Object, 0, len(list))
	for _, it := range list {
		out = append(out, Object{ID: it.Id, Name: it.Name, Labels: it.Labels})
	}
	return out, nil
}

// Volumes lists the volumes of one project (all when project is empty).
func (c *Client) Volumes(ctx context.Context, project string) ([]Object, error) {
	var resp struct {
		Volumes []struct {
			Name   string            `json:"Name"`
			Labels map[string]string `json:"Labels"`
		} `json:"Volumes"`
	}
	if err := c.getJSON(ctx, "/volumes", projectFilter(project), nil, &resp); err != nil {
		return nil, err
	}
	out := make([]Object, 0, len(resp.Volumes))
	for _, v := range resp.Volumes {
		out = append(out, Object{ID: v.Name, Name: v.Name, Labels: v.Labels})
	}
	return out, nil
}

// Images lists the images of one project (all when project is empty).
func (c *Client) Images(ctx context.Context, project string) ([]Object, error) {
	var list []struct {
		Id       string            `json:"Id"`
		RepoTags []string          `json:"RepoTags"`
		Labels   map[string]string `json:"Labels"`
	}
	if err := c.getJSON(ctx, "/images/json", projectFilter(project), nil, &list); err != nil {
		return nil, err
	}
	out := make([]Object, 0, len(list))
	for _, it := range list {
		name := it.Id
		if len(it.RepoTags) > 0 {
			name = it.RepoTags[0]
		}
		out = append(out, Object{ID: it.Id, Name: name, RepoTags: it.RepoTags, Labels: it.Labels, Untagged: untagged(it.RepoTags)})
	}
	return out, nil
}

func projectFilter(project string) map[string][]string {
	if project == "" {
		return nil
	}
	return map[string][]string{"label": {ProjectLabel + "=" + project}}
}

// OwnContainer inspects the container the /proc mountinfo says dev-lease
// lives in and returns its labels (the top-level and the config labels
// merged, config winning) together with the full id the daemon answered
// with (the request may have used a prefix). The daemon's answer is
// double-checked against the requested id: an answer for a different
// container is refused.
func (c *Client) OwnContainer(ctx context.Context, ownID string) (map[string]string, string, error) {
	var inspect struct {
		Id     string            `json:"Id"`
		Labels map[string]string `json:"Labels"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	body, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(ownID)+"/json")
	if err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal(body, &inspect); err != nil {
		return nil, "", err
	}
	if inspect.Id != "" && !strings.HasPrefix(inspect.Id, ownID) {
		return nil, "", fmt.Errorf("the daemon's answer for the own container carries id %s, which does not match the detected id %s", shortID(inspect.Id), shortID(ownID))
	}
	labels := make(map[string]string, len(inspect.Labels)+len(inspect.Config.Labels))
	for k, v := range inspect.Labels {
		labels[k] = v
	}
	for k, v := range inspect.Config.Labels {
		labels[k] = v
	}
	return labels, inspect.Id, nil
}

// OwnProject reads the com.docker.compose.project label of one container.
func (c *Client) OwnProject(ctx context.Context, ownID string) (string, error) {
	labels, _, err := c.OwnContainer(ctx, ownID)
	if err != nil {
		return "", err
	}
	return labels[ProjectLabel], nil
}

// shortID truncates an id for error messages, like docker does.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// StopContainer stops one container, waiting up to timeout. An already
// stopped container (304) counts as success. The request carries its own
// budget (timeout + margin) so a raised --stop-timeout is not cut off by
// the client backstop.
func (c *Client) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout+c.stopMargin)
	defer cancel()
	secs := int(timeout / time.Second)
	_, err := c.doWith(ctx, c.stopHTTP, http.MethodPost, fmt.Sprintf("/containers/%s/stop?t=%d", url.PathEscape(id), secs))
	var se *StatusError
	if errors.As(err, &se) && se.Status == 304 {
		return nil
	}
	return err
}

// RemoveContainer removes one container with anonymous volumes (v=1) and
// without force (force=0).
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/containers/%s?v=1&force=0", url.PathEscape(id)))
	return err
}

// RemoveNetwork removes one network.
func (c *Client) RemoveNetwork(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/networks/"+url.PathEscape(id))
	return err
}

// RemoveVolume removes one volume.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	_, err := c.do(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name))
	return err
}

// ImageID returns the id of the image an id or a repository tag names
// now (GET /images/<ref>/json).
func (c *Client) ImageID(ctx context.Context, ref string) (string, error) {
	body, err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json")
	if err != nil {
		return "", err
	}
	var v struct {
		Id string `json:"Id"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", err
	}
	// An answer without an id never matches a listed id, so a removal
	// skips the tag.
	return v.Id, nil
}

// RemoveImage removes one image by id, or one repository tag of an image
// by its name: the daemon untags an image that has other tags, and
// removes the image with its last tag.
func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	_, err := c.do(ctx, http.MethodDelete, "/images/"+url.PathEscape(ref))
	return err
}

// getJSON performs a list call with an optional label filter and extra query
// parameters.
func (c *Client) getJSON(ctx context.Context, path string, filter map[string][]string, extra url.Values, out any) error {
	p := path
	q := url.Values{}
	if len(filter) > 0 {
		raw, err := json.Marshal(filter)
		if err != nil {
			return err
		}
		q.Set("filters", string(raw))
	}
	for k, vs := range extra {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	body, err := c.do(ctx, http.MethodGet, p)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// do runs one request through the allowlist and the versioned API path
// with the shared client.
func (c *Client) do(ctx context.Context, method, path string) ([]byte, error) {
	return c.doWith(ctx, c.http, method, path)
}

// doWith is do with an explicit client: stop requests use stopHTTP, whose
// budget comes from the request context instead of a fixed timeout.
func (c *Client) doWith(ctx context.Context, hc *http.Client, method, path string) ([]byte, error) {
	version, err := c.version(ctx)
	if err != nil {
		return nil, err
	}
	full := "/v" + version + path
	return c.requestWith(ctx, hc, method, full)
}

func (c *Client) version(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.apiVersion != "" {
		return c.apiVersion, nil
	}
	body, err := c.request(ctx, http.MethodGet, "/version")
	if err != nil {
		return "", err
	}
	var v struct {
		ApiVersion string `json:"ApiVersion"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", err
	}
	m := apiVersionRe.FindStringSubmatch(v.ApiVersion)
	if m == nil {
		return "", fmt.Errorf("docker: cannot parse API version %q", v.ApiVersion)
	}
	c.apiVersion = m[1] + "." + m[2]
	return c.apiVersion, nil
}

func (c *Client) request(ctx context.Context, method, path string) ([]byte, error) {
	return c.requestWith(ctx, c.http, method, path)
}

// requestWith is request with an explicit client.
func (c *Client) requestWith(ctx context.Context, hc *http.Client, method, path string) ([]byte, error) {
	if !allowed(method, path) {
		return nil, fmt.Errorf("%w: %s %s", ErrNotAllowed, method, path)
	}
	// Inspect is for dev-lease's own container only.
	if c.ownID != "" {
		if m := ownInspectRe.FindStringSubmatch(path); m != nil && m[1] != c.ownID {
			return nil, fmt.Errorf("%w: inspecting container %q, only own %q", ErrNotAllowed, m[1], c.ownID)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &StatusError{Method: method, Path: path, Status: resp.StatusCode}
	}
	return body, nil
}

var ownInspectRe = regexp.MustCompile(`^/v\d+\.\d+/containers/([^/]+)/json$`)

// SortedProjects returns the sorted project names of a Projects result.
func SortedProjects(projects map[string]bool) []string {
	out := make([]string, 0, len(projects))
	for p := range projects {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// untagged reports whether the tag list is empty; daemons older than 24
// report a literal "<none>:<none>" entry instead of an empty list.
func untagged(tags []string) bool {
	if len(tags) == 0 {
		return true
	}
	return len(tags) == 1 && tags[0] == "<none>:<none>"
}

// maxLogLineBytes is the longest text one log line keeps: a single
// pathological line must not blow up the page.
const maxLogLineBytes = 2000

// maxLogTimestampBytes is the longest RFC3339Nano timestamp prefix a log
// line can carry; the raw line buffer is capped at
// maxLogLineBytes+maxLogTimestampBytes so the kept 2000 text bytes always
// fit next to the timestamp.
const maxLogTimestampBytes = 36

// maxLogBodyBytes stops reading one logs body after 16 MiB: a runaway
// container must not take serve down through its logs. When the cap falls
// in the middle of one huge line, the newer lines behind it are not read
// and stay unshown: the body's first 16 MiB wins over its tail.
const maxLogBodyBytes = 16 << 20

// ContainerLogs reads the last tail lines of one container's log (stdout
// and stderr, timestamped). The daemon answers with a multiplexed stream
// (8-byte headers, stream byte 1 or 2), or a raw stream for a container
// with tty: true; a stream without the multiplexed content type is sniffed
// from its first bytes. Every line's text is cut at maxLogLineBytes bytes
// and invalid UTF-8 is replaced. tail is clamped to 1..200.
func (c *Client) ContainerLogs(ctx context.Context, id string, tail int) ([]LogLine, error) {
	if tail < 1 {
		tail = 1
	}
	if tail > 200 {
		tail = 200
	}
	body, contentType, err := c.logsStream(ctx, "/containers/"+url.PathEscape(id)+"/logs?stdout=1&stderr=1&timestamps=1&tail="+strconv.Itoa(tail))
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return decodeLogs(body, contentType, tail), nil
}

// logsStream runs the one logs request and returns the body, capped at
// maxLogBodyBytes, with its content type. The body must be closed.
func (c *Client) logsStream(ctx context.Context, path string) (io.ReadCloser, string, error) {
	version, err := c.version(ctx)
	if err != nil {
		return nil, "", err
	}
	full := "/v" + version + path
	if !allowed(http.MethodGet, full) {
		return nil, "", fmt.Errorf("%w: %s %s", ErrNotAllowed, http.MethodGet, path)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+full, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		resp.Body.Close()
		return nil, "", &StatusError{Method: http.MethodGet, Path: path, Status: resp.StatusCode}
	}
	body := struct {
		io.Reader
		io.Closer
	}{io.LimitReader(resp.Body, maxLogBodyBytes), resp.Body}
	return body, resp.Header.Get("Content-Type"), nil
}

// decodeLogs walks one logs stream into the last tail lines. Multiplexed
// frames (8-byte header, stream byte 1 or 2, then a big-endian uint32
// length) may cut a line anywhere; a frame with another stream byte is
// skipped with its whole payload. A raw (TTY) stream is split on newlines.
// The body may stop at any point (the cap, or a truncated final frame): a
// trailing partial line is kept. Every line is ring-buffered and cut at
// maxLogLineBytes while it streams past, so the reader never holds more
// than tail cut lines.
func decodeLogs(r io.Reader, contentType string, tail int) []LogLine {
	br := bufio.NewReader(r)
	multiplexed := contentType == "application/vnd.docker.multiplexed-stream"
	if !multiplexed {
		if head, _ := br.Peek(8); len(head) == 8 && head[0] <= 3 && head[1] == 0 && head[2] == 0 && head[3] == 0 {
			multiplexed = true
		}
	}
	if !multiplexed {
		return decodeRaw(br, tail)
	}

	var lines []LogLine
	keep := logRing(&lines, tail)
	sinks := [3]*lineSink{nil, newLineSink(1, keep), newLineSink(2, keep)}

	var hdr [8]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			break // clean end at a frame boundary, or a truncated header
		}
		stream := hdr[0]
		size := int(binary.BigEndian.Uint32(hdr[4:8]))
		if stream != 1 && stream != 2 {
			if _, err := io.CopyN(io.Discard, br, int64(size)); err != nil {
				break // the body ended inside the skipped payload
			}
			continue
		}
		if err := readFramePayload(br, size, sinks[stream].write); err != nil {
			break // the body ended inside the frame: its partial line is kept
		}
	}
	// A trailing partial line is kept.
	sinks[1].flush()
	sinks[2].flush()
	return lines
}

// decodeRaw splits a raw (TTY) stream on newlines: every line is stdout,
// cut and ring-buffered the same way. TTY streams end lines with \r\n;
// the carriage return is dropped so the text holds the line only.
func decodeRaw(r io.Reader, tail int) []LogLine {
	var lines []LogLine
	keep := logRing(&lines, tail)
	sink := newLineSink(1, func(ll LogLine) {
		ll.Text = strings.TrimSuffix(ll.Text, "\r")
		keep(ll)
	})
	br := bufio.NewReader(r)
	for {
		chunk, err := br.ReadSlice('\n')
		sink.write(chunk)
		switch {
		case err == nil:
			// found the newline, keep going
		case errors.Is(err, bufio.ErrBufferFull):
			// the line continues past the buffer
		default:
			// EOF or the body cap: a trailing partial line is kept
			sink.flush()
			return lines
		}
	}
}

// logRing returns the keep function both decoders share: it holds the last
// tail lines in lines (tail is at least 1).
func logRing(lines *[]LogLine, tail int) func(LogLine) {
	return func(ll LogLine) {
		if tail == 1 {
			*lines = append((*lines)[:0], ll)
			return
		}
		*lines = append(*lines, ll)
		if len(*lines) > tail {
			*lines = (*lines)[1:]
		}
	}
}

// lineSink assembles one stream's lines from arbitrary chunks, cutting
// each raw line at maxLogLineBytes+maxLogTimestampBytes: the rest of an
// over-long line is dropped while it streams past, but a newline still
// ends it so later lines are kept.
type lineSink struct {
	stream byte
	keep   func(LogLine)
	buf    []byte
	cut    bool // buf is at the cap: the rest of this line is dropped
}

func newLineSink(stream byte, keep func(LogLine)) *lineSink {
	return &lineSink{stream: stream, keep: keep}
}

func (s *lineSink) write(chunk []byte) {
	cap := maxLogLineBytes + maxLogTimestampBytes
	for len(chunk) > 0 {
		if s.cut {
			if i := bytes.IndexByte(chunk, '\n'); i < 0 {
				return
			} else {
				chunk = chunk[i+1:]
				s.flush()
				continue
			}
		}
		i := bytes.IndexByte(chunk, '\n')
		seg, rest := chunk, []byte(nil)
		if i >= 0 {
			seg, rest = chunk[:i], chunk[i+1:]
		}
		if room := cap - len(s.buf); len(seg) >= room {
			s.buf = append(s.buf, seg[:room]...)
			s.cut = true
		} else {
			s.buf = append(s.buf, seg...)
		}
		chunk = rest
		if i >= 0 {
			s.flush()
		}
	}
}

// flush emits the buffered line and resets the sink.
func (s *lineSink) flush() {
	if len(s.buf) > 0 {
		s.keep(parseLogLine(s.buf, s.stream))
		s.buf = s.buf[:0]
	}
	s.cut = false
}

// readFramePayload reads exactly size bytes in bounded chunks, feeding
// each chunk to f; a body ending inside the frame returns the error and
// feeds nothing more.
func readFramePayload(r io.Reader, size int, f func([]byte)) error {
	var buf [32 << 10]byte
	for size > 0 {
		n := len(buf)
		if size < n {
			n = size
		}
		got, err := r.Read(buf[:n])
		if got > 0 {
			f(buf[:got])
			size -= got
		}
		if err != nil {
			if err == io.EOF && size == 0 {
				return nil
			}
			return err
		}
	}
	return nil
}

// parseLogLine splits the leading RFC3339Nano timestamp off one raw line,
// cuts the text and replaces invalid UTF-8.
func parseLogLine(raw []byte, stream byte) LogLine {
	ll := LogLine{Stream: "stderr"}
	if stream == 1 {
		ll.Stream = "stdout"
	}
	if i := bytes.IndexByte(raw, ' '); i > 0 {
		if t, err := time.Parse(time.RFC3339Nano, string(raw[:i])); err == nil {
			ll.Time = t.UTC()
			raw = raw[i+1:]
		}
	}
	if len(raw) > maxLogLineBytes {
		raw = raw[:maxLogLineBytes]
	}
	ll.Text = strings.ToValidUTF8(string(raw), "\uFFFD")
	return ll
}

// ContainerMemory reads one container's memory use in bytes from the
// one-shot stats endpoint: memory_stats.usage minus
// memory_stats.stats.inactive_file when present (inactive file cache is
// evictable and must not count as used memory).
func (c *Client) ContainerMemory(ctx context.Context, id string) (uint64, error) {
	body, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/stats?stream=false&one-shot=true")
	if err != nil {
		return 0, err
	}
	var stats struct {
		MemoryStats struct {
			Usage uint64 `json:"usage"`
			Stats struct {
				InactiveFile *uint64 `json:"inactive_file"`
			} `json:"stats"`
		} `json:"memory_stats"`
	}
	if err := json.Unmarshal(body, &stats); err != nil {
		return 0, err
	}
	usage := stats.MemoryStats.Usage
	if inactive := stats.MemoryStats.Stats.InactiveFile; inactive != nil {
		if *inactive >= usage {
			return 0, nil
		}
		usage -= *inactive
	}
	return usage, nil
}

// volumeSizesTTL bounds the volume size cache: one /system/df call per
// minute at most, whatever the number of readers.
const volumeSizesTTL = time.Minute

// VolumeSizes maps every volume name to its Size in bytes. A volume with
// no UsageData or a negative Size (the daemon's "cannot compute") is left
// out. The answer is cached per client for volumeSizesTTL.
func (c *Client) VolumeSizes(ctx context.Context) (map[string]int64, error) {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	if c.sizes != nil && time.Since(c.sizesAt) < volumeSizesTTL {
		return cloneSizes(c.sizes), nil
	}
	body, err := c.do(ctx, http.MethodGet, "/system/df?type=volume")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Volumes []struct {
			Name      string `json:"Name"`
			UsageData *struct {
				Size int64 `json:"Size"`
			} `json:"UsageData"`
		} `json:"Volumes"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	sizes := make(map[string]int64, len(resp.Volumes))
	for _, v := range resp.Volumes {
		if v.Name == "" || v.UsageData == nil || v.UsageData.Size < 0 {
			continue
		}
		sizes[v.Name] = v.UsageData.Size
	}
	c.sizes = sizes
	c.sizesAt = time.Now()
	return cloneSizes(sizes), nil
}

func cloneSizes(sizes map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(sizes))
	for name, size := range sizes {
		out[name] = size
	}
	return out
}
