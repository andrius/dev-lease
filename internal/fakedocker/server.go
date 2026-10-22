// Package fakedocker is an in-memory test double of the Docker Engine API
// served over a Unix socket. It keeps containers, networks, volumes and
// images labelled with com.docker.compose.project, records every call it
// received and can refuse operations on single objects.
package fakedocker

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const apiVersion = "1.47"

const projectLabel = "com.docker.compose.project"

// Call is one request the fake daemon received.
type Call struct {
	Method string
	Path   string
	Query  url.Values
	Status int
}

type obj struct {
	id              string
	name            string
	labels          map[string]string
	running         bool      // containers only
	imageID         string    // containers only
	imageRef        string    // containers only: the listing's Image reference
	tty             bool      // containers only: logs are a raw stream
	inspectID       string    // containers only: Id answered by inspect
	untagged        bool      // images only: no repository tag left
	networks        []string  // containers only: attached network names
	mounts          []string  // containers only: mounted named volumes
	created         time.Time // containers only: listing Created (Unix seconds)
	logs            []byte    // containers only: raw multiplexed log bytes
	logsWriteFailed bool      // containers only: the last logs write failed mid-body
	memUsage        uint64    // containers only: memory_stats.usage
	memInact        uint64    // containers only: memory_stats.stats.inactive_file
	haveMem         bool      // containers only: SetMemory ran
	size            int64     // volumes only: UsageData.Size
	haveSize        bool      // volumes only: SetVolumeSize ran
}

// createdAt is the fixed creation time containers report in their listing;
// docker/client_test.go pins the same value, keep the two in sync.
var createdAt = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// Server is the fake daemon.
type Server struct {
	srv    *httptest.Server
	Socket string

	mu           sync.Mutex
	containers   map[string]*obj
	networks     map[string]*obj
	volumes      map[string]*obj
	images       map[string]*obj
	refuse       map[string]int // "kind:id" -> HTTP status to return
	beforeRemove map[string]func()
	redirects    map[string]string // path -> Location
	onRequest    func(method, path string)
	calls        []Call
}

// New starts a fake daemon on a fresh Unix socket in the test's temp dir.
func New(t *testing.T) *Server {
	t.Helper()
	return NewAt(t, filepath.Join(t.TempDir(), "docker.sock"))
}

// NewAt starts a fake daemon on the given socket path.
func NewAt(t *testing.T, socketPath string) *Server {
	t.Helper()
	s := &Server{
		containers:   map[string]*obj{},
		networks:     map[string]*obj{},
		volumes:      map[string]*obj{},
		images:       map[string]*obj{},
		refuse:       map[string]int{},
		beforeRemove: map[string]func(){},
		redirects:    map[string]string{},
	}
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.handle))
	s.srv.Listener = l
	s.srv.Start()
	s.Socket = socketPath
	t.Cleanup(func() {
		s.srv.Close()
	})
	return s
}

// Close shuts the fake daemon down immediately.
func (s *Server) Close() {
	s.srv.Close()
}

// Add creates an object of the given kind (container, network, volume or
// image) labelled with project.
func (s *Server) Add(kind, id, name, project string) {
	s.AddLabels(kind, id, name, map[string]string{projectLabel: project})
}

// AddLabels creates an object with arbitrary labels. Volumes are identified
// by name in the Engine API, so the name is also the volume's id.
// Containers start running and with no image.
func (s *Server) AddLabels(kind, id, name string, labels map[string]string) {
	if kind == "volume" {
		id = name
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == "container" {
		s.set(kind)[id] = &obj{id: id, name: name, labels: labels, running: true, created: createdAt}
		return
	}
	s.set(kind)[id] = &obj{id: id, name: name, labels: labels}
}

// Drop removes an object of the given kind (container, network, volume or
// image) directly, like a daemon-side deletion outside dev-lease's API: a
// compose down, a manual docker rm. No status is answered and nothing is
// refused.
func (s *Server) Drop(kind, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.set(kind), id)
}

// SetContainerLabels replaces the label set of one container; keys set to
// "" are dropped.
func (s *Server) SetContainerLabels(id string, labels map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.set("container")[id]
	for k, v := range labels {
		if v == "" {
			delete(o.labels, k)
		} else {
			o.labels[k] = v
		}
	}
}

// AddUntaggedImage creates an image with no repository tag left (compose
// up --build orphans the old tag).
func (s *Server) AddUntaggedImage(id, project string) {
	s.AddLabels("image", id, id, map[string]string{projectLabel: project})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.images[id].untagged = true
}

// AddContainerWithImage creates a running container that was created from
// the image with the given id, so an image in use can be modelled. The id
// also serves as the listing's Image reference unless one is set with
// SetContainerImageRef.
func (s *Server) AddContainerWithImage(id, name, project, imageID string) {
	s.AddContainerWithUses(id, name, map[string]string{projectLabel: project}, imageID, nil, nil)
}

// AddContainerWithUses creates a running container with arbitrary labels,
// created from the image with the given id, attached to the named networks
// and mounting the named volumes (NetworkSettings.Networks and Mounts in
// the listing).
func (s *Server) AddContainerWithUses(id, name string, labels map[string]string, imageID string, networks, mounts []string) {
	s.AddLabels("container", id, name, labels)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.containers[id].imageID = imageID
	s.containers[id].imageRef = imageID
	s.containers[id].networks = append([]string(nil), networks...)
	s.containers[id].mounts = append([]string(nil), mounts...)
}

// SetContainerImageRef sets the image reference the container was created
// from (the listing's Image field).
func (s *Server) SetContainerImageRef(id, ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.containers[id].imageRef = ref
}

// SetTTY marks one container as tty: true: its logs are a raw stream with
// no frame headers.
func (s *Server) SetTTY(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.containers[id].tty = true
}

// StopContainer flips one container to stopped, as a daemon stop would.
func (s *Server) StopContainer(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.containers[id]; ok {
		o.running = false
	}
}

// StartContainer marks a stopped container running again, like
// `docker compose start`.
func (s *Server) StartContainer(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.containers[id]; ok {
		o.running = true
	}
}

// IsStopped reports whether a container exists and is stopped.
func (s *Server) IsStopped(kind, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case "container":
		o, ok := s.containers[id]
		return ok && !o.running
	}
	return false
}

// SetInspectID makes GET /containers/<id>/json answer with the given Id
// field while the container stays stored under id. It lets tests verify
// that a mismatching answer is refused.
func (s *Server) SetInspectID(id, inspectID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.containers[id]; ok {
		s.containers[id].inspectID = inspectID
	}
}

// SetLogs stores the raw multiplexed bytes GET /containers/<id>/logs
// answers with.
func (s *Server) SetLogs(id string, raw []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.containers[id]; ok {
		o.logs = append([]byte(nil), raw...)
	}
}

// SetMemory stores the one-shot memory stats of a container: usage and the
// inactive file cache. The inactive_file key is answered once SetMemory ran.
func (s *Server) SetMemory(id string, usage, inactive uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.containers[id]; ok {
		o.memUsage, o.memInact, o.haveMem = usage, inactive, true
	}
}

// SetVolumeSize stores the UsageData.Size of a volume, creating the volume
// bare if it does not exist yet. A negative size is the daemon's "cannot
// compute".
func (s *Server) SetVolumeSize(name string, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.volumes[name]
	if !ok {
		o = &obj{id: name, name: name}
		s.volumes[name] = o
	}
	o.size, o.haveSize = size, true
}

// Redirect makes GET requests to path answer 302 with the given Location.
func (s *Server) Redirect(path, location string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redirects[path] = location
}

// OnRequest registers a hook run on every request, before routing. It lets
// tests land a concurrent action at a precise point of a pass.
func (s *Server) OnRequest(fn func(method, path string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onRequest = fn
}

// Refuse makes every stop/remove call on the object return the given status.
func (s *Server) Refuse(kind, id string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse[kind+":"+id] = status
}

// RefuseStop makes POST stop calls on the object return the given status
// while other calls on it keep working.
func (s *Server) RefuseStop(kind, id string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse["stop:"+kind+":"+id] = status
}

// FailList makes every GET list call of the kind answer the given status.
func (s *Server) FailList(kind string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse["list:"+kind] = status
}

// UnfailList clears a list failure.
func (s *Server) UnfailList(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refuse, "list:"+kind)
}

// Unrefuse clears a refusal (a stop-only one included).
func (s *Server) Unrefuse(kind, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refuse, kind+":"+id)
	delete(s.refuse, "stop:"+kind+":"+id)
}

// BeforeRemove registers fn to run (once per matching call) before the object
// is stopped or removed.
func (s *Server) BeforeRemove(kind, id string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beforeRemove[kind+":"+id] = fn
}

// Calls returns a copy of the recorded calls.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Call, len(s.calls))
	copy(out, s.calls)
	return out
}

// Has reports whether the object still exists.
func (s *Server) Has(kind, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.set(kind)[id]
	return ok
}

// Count returns how many objects of the kind exist.
func (s *Server) Count(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.set(kind))
}

func (s *Server) set(kind string) map[string]*obj {
	switch kind {
	case "container":
		return s.containers
	case "network":
		return s.networks
	case "volume":
		return s.volumes
	case "image":
		return s.images
	}
	panic("fakedocker: unknown kind " + kind)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	fn := s.onRequest
	s.mu.Unlock()
	if fn != nil {
		fn(r.Method, r.URL.Path)
	}
	status := s.route(w, r)
	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Status: status})
	s.mu.Unlock()
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) int {
	p := r.URL.Path
	if r.Method == http.MethodGet && p == "/version" {
		writeJSON(w, http.StatusOK, map[string]any{"ApiVersion": apiVersion, "Version": "29.9.1"})
		return http.StatusOK
	}
	s.mu.Lock()
	redirectTo, redirected := s.redirects[r.URL.Path]
	s.mu.Unlock()
	if r.Method == http.MethodGet && redirected {
		http.Redirect(w, r, redirectTo, http.StatusFound)
		return http.StatusFound
	}
	// Paths are /v<major>.<minor>/<kind>[/<id>[/<op>]].
	if !strings.HasPrefix(p, "/v") {
		return notFound(w)
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) < 2 {
		return notFound(w)
	}
	if parts[0] != "v"+apiVersion {
		return notFound(w)
	}
	kind := parts[1]
	id, op := "", ""
	if len(parts) > 2 {
		id = parts[2]
	}
	if len(parts) > 3 {
		op = parts[3]
	}

	switch r.Method {
	case http.MethodGet:
		switch {
		case kind == "containers" && op == "":
			return s.listContainers(w, r)
		case kind == "containers" && op == "json":
			return s.inspectContainer(w, id)
		case kind == "containers" && op == "logs":
			return s.containerLogs(w, r, id)
		case kind == "containers" && op == "stats":
			return s.containerStats(w, id)
		case kind == "system" && id == "df":
			return s.systemDf(w)
		case kind == "networks" && op == "":
			return s.list(w, s.networks, r, "network")
		case kind == "volumes" && op == "":
			return s.listVolumes(w, r)
		case kind == "images" && op == "":
			return s.list(w, s.images, r, "image")
		}
	case http.MethodPost:
		if kind == "containers" && op == "stop" {
			return s.stopOrRemove(w, r, "container", id, false)
		}
	case http.MethodDelete:
		switch kind {
		case "containers":
			return s.stopOrRemove(w, r, "container", id, true)
		case "networks":
			return s.delete(w, r, "network", id)
		case "volumes":
			return s.delete(w, r, "volume", id)
		case "images":
			return s.delete(w, r, "image", id)
		}
	}
	return notFound(w)
}

func (s *Server) listStatus(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refuse["list:"+kind]
}

func (s *Server) listContainers(w http.ResponseWriter, r *http.Request) int {
	if status := s.listStatus("container"); status != 0 {
		writeJSON(w, status, map[string]string{"message": "refused"})
		return status
	}
	filter := parseFilters(r)
	all := r.URL.Query().Get("all") == "1"
	objs := s.matching(s.containers, filter)
	type cont struct {
		Id              string            `json:"Id"`
		Names           []string          `json:"Names"`
		ImageID         string            `json:"ImageID"`
		Image           string            `json:"Image"`
		Labels          map[string]string `json:"Labels"`
		State           string            `json:"State"`
		Created         int64             `json:"Created"`
		Status          string            `json:"Status"`
		NetworkSettings struct {
			Networks map[string]struct{} `json:"Networks"`
		} `json:"NetworkSettings"`
		Mounts []struct {
			Type string `json:"Type"`
			Name string `json:"Name"`
		} `json:"Mounts"`
	}
	out := make([]cont, 0, len(objs))
	s.mu.Lock()
	for _, o := range objs {
		if !all && !o.running {
			continue
		}
		state := "exited"
		if o.running {
			state = "running"
		}
		nets := map[string]struct{}{}
		for _, n := range o.networks {
			nets[n] = struct{}{}
		}
		mounts := make([]struct {
			Type string `json:"Type"`
			Name string `json:"Name"`
		}, 0, len(o.mounts))
		for _, m := range o.mounts {
			mounts = append(mounts, struct {
				Type string `json:"Type"`
				Name string `json:"Name"`
			}{Type: "volume", Name: m})
		}
		status := "Exited (0)"
		if o.running {
			status = "Up"
		}
		out = append(out, cont{Id: o.id, Names: []string{"/" + o.name}, ImageID: o.imageID, Image: o.imageRef, Labels: o.labels,
			State: state, Created: o.created.Unix(), Status: status,
			NetworkSettings: struct {
				Networks map[string]struct{} `json:"Networks"`
			}{Networks: nets}, Mounts: mounts})
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
	return http.StatusOK
}

func (s *Server) inspectContainer(w http.ResponseWriter, id string) int {
	s.mu.Lock()
	o, ok := s.containers[id]
	s.mu.Unlock()
	if !ok {
		return notFound(w)
	}
	inspectID := o.inspectID
	if inspectID == "" {
		inspectID = o.id
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Id":     inspectID,
		"Labels": o.labels,
		"Config": map[string]any{"Labels": o.labels},
	})
	return http.StatusOK
}

func (s *Server) listVolumes(w http.ResponseWriter, r *http.Request) int {
	if status := s.listStatus("volume"); status != 0 {
		writeJSON(w, status, map[string]string{"message": "refused"})
		return status
	}
	filter := parseFilters(r)
	objs := s.matching(s.volumes, filter)
	type vol struct {
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
	}
	out := make([]vol, 0, len(objs))
	for _, o := range objs {
		out = append(out, vol{Name: o.id, Labels: o.labels})
	}
	writeJSON(w, http.StatusOK, map[string]any{"Volumes": out, "Warnings": []string{}})
	return http.StatusOK
}

func (s *Server) list(w http.ResponseWriter, set map[string]*obj, r *http.Request, kind string) int {
	if status := s.listStatus(kind); status != 0 {
		writeJSON(w, status, map[string]string{"message": "refused"})
		return status
	}
	filter := parseFilters(r)
	objs := s.matching(set, filter)
	type item struct {
		Id       string            `json:"Id"`
		Name     string            `json:"Name"`
		RepoTags []string          `json:"RepoTags,omitempty"`
		Labels   map[string]string `json:"Labels"`
	}
	out := make([]item, 0, len(objs))
	for _, o := range objs {
		it := item{Id: o.id, Name: o.name, Labels: o.labels}
		if strings.HasPrefix(o.id, "sha256:") && !o.untagged {
			it.RepoTags = []string{o.name}
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, out)
	return http.StatusOK
}

func (s *Server) stopOrRemove(w http.ResponseWriter, r *http.Request, kind, id string, isDelete bool) int {
	s.mu.Lock()
	o, ok := s.set(kind)[id]
	status := s.refuse[kind+":"+id]
	if !isDelete {
		if st := s.refuse["stop:"+kind+":"+id]; st != 0 {
			status = st
		}
	}
	fn := s.beforeRemove[kind+":"+id]
	s.mu.Unlock()
	if !ok {
		return notFound(w)
	}
	if status != 0 {
		writeJSON(w, status, map[string]string{"message": "refused"})
		return status
	}
	if fn != nil {
		fn()
	}
	if kind == "container" {
		s.mu.Lock()
		running := o.running
		s.mu.Unlock()
		if !isDelete {
			// stop on an already stopped container answers 304.
			if !running {
				w.WriteHeader(http.StatusNotModified)
				return http.StatusNotModified
			}
			s.mu.Lock()
			o.running = false
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return http.StatusNoContent
		}
		// remove on a running container is refused without force.
		if running && r.URL.Query().Get("force") != "1" {
			writeJSON(w, http.StatusConflict, map[string]string{"message": "container is running"})
			return http.StatusConflict
		}
	}
	if kind == "image" {
		// An image any container still references cannot be removed.
		s.mu.Lock()
		inUse := false
		for _, c := range s.containers {
			if c.imageID == id {
				inUse = true
				break
			}
		}
		s.mu.Unlock()
		if inUse {
			writeJSON(w, http.StatusConflict, map[string]string{"message": "image is in use"})
			return http.StatusConflict
		}
	}
	if isDelete {
		s.mu.Lock()
		delete(s.set(kind), id)
		s.mu.Unlock()
	}
	_ = o
	w.WriteHeader(http.StatusNoContent)
	return http.StatusNoContent
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request, kind, id string) int {
	return s.stopOrRemove(w, r, kind, id, true)
}

func (s *Server) matching(set map[string]*obj, filter map[string][]string) []*obj {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := filter["label"]
	out := make([]*obj, 0, len(set))
	for _, o := range set {
		if len(values) == 0 || matchLabels(o.labels, values) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func matchLabels(labels map[string]string, values []string) bool {
	for _, v := range values {
		if k, val, ok := strings.Cut(v, "="); ok {
			if labels[k] == val {
				return true
			}
		} else if _, exists := labels[v]; exists {
			return true
		}
	}
	return false
}

func parseFilters(r *http.Request) map[string][]string {
	raw := r.URL.Query().Get("filters")
	if raw == "" {
		return nil
	}
	var m map[string][]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

func notFound(w http.ResponseWriter) int {
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such object"})
	return http.StatusNotFound
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// containerLogs answers the stored bytes of GET /containers/<id>/logs,
// like a real daemon: a multiplexed stream for a normal container, a raw
// stream with no headers for a container marked TTY. Only the last tail
// lines are answered, like the daemon's own tail cut.
func (s *Server) containerLogs(w http.ResponseWriter, r *http.Request, id string) int {
	s.mu.Lock()
	o, ok := s.containers[id]
	s.mu.Unlock()
	if !ok {
		return notFound(w)
	}
	ctype := "application/vnd.docker.multiplexed-stream"
	var payload []byte
	if o.tty {
		ctype = "application/vnd.docker.raw-stream"
		payload = rawLogTail(o.logs, tailFromQuery(r))
	} else {
		payload = muxLogTail(o.logs, tailFromQuery(r))
	}
	w.Header().Set("Content-Type", ctype)
	w.WriteHeader(http.StatusOK)
	_, writeErr := w.Write(payload)
	s.mu.Lock()
	s.containers[id].logsWriteFailed = writeErr != nil
	s.mu.Unlock()
	return http.StatusOK
}

// tailFromQuery reads the logs request's tail parameter and clamps it to
// 1..200 like the client does.
func tailFromQuery(r *http.Request) int {
	tail, err := strconv.Atoi(r.URL.Query().Get("tail"))
	if err != nil || tail < 1 {
		tail = 1
	}
	if tail > 200 {
		tail = 200
	}
	return tail
}

// muxLogTail returns the suffix of a multiplexed stream that holds its
// last tail lines, without copying the bytes. The payload bytes in
// arrival order are the container's log file, so the cut falls on the
// file's line boundaries; a line spanning frames of its stream stays
// whole, and a truncated trailing frame is kept as the client expects.
func muxLogTail(raw []byte, tail int) []byte {
	var lineStart []int // raw offset where each line's first frame begins
	curStart := -1
	for rest := raw; len(rest) >= 8; {
		size := int(binary.BigEndian.Uint32(rest[4:8]))
		if len(rest) < 8+size {
			break // a truncated trailing frame: kept, like the client reads it
		}
		if curStart < 0 {
			curStart = len(raw) - len(rest)
		}
		if bytes.Contains(rest[8:8+size], []byte{'\n'}) {
			lineStart = append(lineStart, curStart)
			curStart = -1
		}
		rest = rest[8+size:]
	}
	if len(lineStart) > tail {
		return raw[lineStart[len(lineStart)-tail]:]
	}
	if len(lineStart) > 0 {
		return raw[lineStart[0]:]
	}
	if curStart >= 0 {
		return raw[curStart:]
	}
	return nil
}

// rawLogTail cuts a raw (TTY) stream to its last tail lines, keeping the
// stream's line endings.
func rawLogTail(raw []byte, tail int) []byte {
	trailingNL := bytes.HasSuffix(raw, []byte{'\n'})
	lines := bytes.Split(raw, []byte{'\n'})
	if trailingNL {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	out := bytes.Join(lines, []byte{'\n'})
	if trailingNL {
		out = append(out, '\n')
	}
	return out
}

// LogsWriteFailed reports whether the last logs write for one container
// failed mid-body: the client stopped reading before the end.
func (s *Server) LogsWriteFailed(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.containers[id].logsWriteFailed
}

// containerStats answers the one-shot memory stats of a container. Without
// SetMemory the answer carries usage 0 and no inactive_file key.
func (s *Server) containerStats(w http.ResponseWriter, id string) int {
	s.mu.Lock()
	o, ok := s.containers[id]
	s.mu.Unlock()
	if !ok {
		return notFound(w)
	}
	mem := map[string]any{"usage": o.memUsage}
	if o.haveMem {
		mem["stats"] = map[string]any{"inactive_file": o.memInact}
	}
	writeJSON(w, http.StatusOK, map[string]any{"memory_stats": mem})
	return http.StatusOK
}

// FailSystemDf makes GET /system/df?type=volume answer the given status
// (the volume size read), so a refused size read can be modelled.
func (s *Server) FailSystemDf(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse["systemdf"] = status
}

// UnfailSystemDf clears a system/df failure.
func (s *Server) UnfailSystemDf() {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refuse, "systemdf")
}

// systemDf answers GET /system/df?type=volume: every known volume with its
// stored size; a volume SetVolumeSize never ran for answers without
// UsageData (the client treats that as unknown).
func (s *Server) systemDf(w http.ResponseWriter) int {
	s.mu.Lock()
	if status := s.refuse["systemdf"]; status != 0 {
		s.mu.Unlock()
		writeJSON(w, status, map[string]string{"message": "refused"})
		return status
	}
	type usage struct {
		Size     int64 `json:"Size"`
		RefCount int   `json:"RefCount"`
	}
	type dfVol struct {
		Name      string `json:"Name"`
		UsageData *usage `json:"UsageData,omitempty"`
	}
	out := make([]dfVol, 0, len(s.volumes))
	for _, o := range s.volumes {
		v := dfVol{Name: o.id}
		if o.haveSize {
			v.UsageData = &usage{Size: o.size}
		}
		out = append(out, v)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"Volumes": out})
	return http.StatusOK
}
