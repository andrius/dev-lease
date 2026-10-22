package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/andrius/dev-lease/internal/control"
	"github.com/andrius/dev-lease/internal/reaper"
	"github.com/andrius/dev-lease/internal/view"
)

// Read-only calls get half a minute; an action may wait on serve for a
// whole removal, so it gets ten minutes. A test lowers actionTimeout.
const readTimeout = 30 * time.Second

var actionTimeout = 10 * time.Minute

// contentItem is one entry of a tools/call result's content array.
type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// callResult is the result object of tools/call. StructuredContent is
// present on the read-only tools; IsError marks a tool error.
type callResult struct {
	Content           []contentItem `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}

// tool is one entry of tools/list.
type tool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	Annotations  map[string]any `json:"annotations,omitempty"`
}

// toolDefsList is the tools/list payload; the schema maps are shared
// with the unit tests that check the structured content against them.
var toolDefsList = []tool{
	{
		Name:        "list_stacks",
		Description: "Only stacks dev-lease manages are visible: list each stack's state, next event (stop or removal), holders, and disk use.",
		InputSchema: obj(nil, map[string]any{}),
		OutputSchema: obj([]string{"stacks"}, map[string]any{
			"stacks": arr(obj([]string{
				"name", "state", "next", "never_remove",
				"containers_running", "containers_total", "disk_bytes", "holders",
			}, map[string]any{
				"name":               str(),
				"path":               str(),
				"state":              strEnum(view.StateLive, view.StateStopped, view.StateStopping, view.StateRemoving, view.StateNew),
				"next":               str(),
				"next_at":            str(),
				"never_remove":       boolean(),
				"containers_running": integer(),
				"containers_total":   integer(),
				"disk_bytes":         integer(),
				"holders": arr(obj([]string{"name", "until"}, map[string]any{
					"name":  str(),
					"until": str(),
				})),
			})),
		}),
		Annotations: map[string]any{"readOnlyHint": true},
	},
	{
		Name:        "stack_details",
		Description: "Only stacks dev-lease manages are visible: detail one stack's containers, volumes, images, disk use, a start command, and the recent logs of one service.",
		InputSchema: obj([]string{"name"}, map[string]any{
			"name":        str(),
			"log_service": str(),
			"log_lines":   integerRange(0, 200),
		}),
		OutputSchema: obj([]string{
			"name", "state", "next", "never_remove",
			"containers_running", "containers_total", "disk_bytes", "holders",
			"containers", "volumes", "removed_images", "kept_images", "networks",
			"start_command", "logs", "log_service",
		}, map[string]any{
			"name":               str(),
			"path":               str(),
			"state":              strEnum(view.StateLive, view.StateStopped, view.StateStopping, view.StateRemoving, view.StateNew),
			"next":               str(),
			"next_at":            str(),
			"never_remove":       boolean(),
			"containers_running": integer(),
			"containers_total":   integer(),
			"disk_bytes":         integer(),
			"holders": arr(obj([]string{"name", "until"}, map[string]any{
				"name":  str(),
				"until": str(),
			})),
			"containers": arr(obj([]string{
				"service", "name", "state", "status", "image", "memory_bytes", "kept",
			}, map[string]any{
				"service":      str(),
				"name":         str(),
				"state":        strEnum("running", "exited"),
				"status":       str(),
				"image":        str(),
				"memory_bytes": integer(),
				"kept":         boolean(),
			})),
			"volumes": arr(obj([]string{"name", "bytes", "kept"}, map[string]any{
				"name":  str(),
				"bytes": integer(),
				"kept":  boolean(),
			})),
			"removed_images": arr(str()),
			"kept_images":    arr(str()),
			"networks":       arr(str()),
			"start_command":  str(),
			"logs": arr(obj([]string{"stream", "text"}, map[string]any{
				"time":   str(), // absent for TTY log lines
				"stream": strEnum("stdout", "stderr"),
				"text":   str(),
			})),
			"log_service": str(),
		}),
		Annotations: map[string]any{"readOnlyHint": true},
	},
	{
		Name:         "stop_stack",
		Description:  "Only stacks dev-lease manages are visible: stop one stack now, its containers are stopped and nothing is deleted. The stack can be started again.",
		InputSchema:  obj([]string{"name"}, map[string]any{"name": str()}),
		OutputSchema: obj([]string{"name"}, map[string]any{"name": str()}),
		Annotations: map[string]any{
			"readOnlyHint":    false,
			"destructiveHint": false,
			"idempotentHint":  true,
		},
	},
	{
		Name:        "remove_stack",
		Description: "Only stacks dev-lease manages are visible: remove one stack, stop it and delete its containers, networks, volumes and images. Deleting data is permanent, so the confirm argument must equal the stack name.",
		InputSchema: obj([]string{"name", "confirm"}, map[string]any{
			"name":    str(),
			"confirm": str(),
		}),
		OutputSchema: obj([]string{"name"}, map[string]any{"name": str()}),
		Annotations: map[string]any{
			"destructiveHint": true,
			"idempotentHint":  false,
		},
	},
}

func toolDefs() []tool { return toolDefsList }

func findTool(name string) *tool {
	for i := range toolDefsList {
		if toolDefsList[i].Name == name {
			return &toolDefsList[i]
		}
	}
	return nil
}

func str() map[string]any { return map[string]any{"type": "string"} }
func strEnum(vals ...string) map[string]any {
	return map[string]any{"type": "string", "enum": vals}
}
func boolean() map[string]any { return map[string]any{"type": "boolean"} }
func integer() map[string]any { return map[string]any{"type": "integer"} }
func integerRange(min, max int) map[string]any {
	return map[string]any{"type": "integer", "minimum": float64(min), "maximum": float64(max)}
}
func arr(items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}
func obj(required []string, props map[string]any) map[string]any {
	// The properties map is always present (empty when there are none),
	// so a client can always walk it.
	m := map[string]any{"type": "object", "additionalProperties": false, "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

// call handles one tools/call: it validates the params, runs the tool
// under a timeout and maps a tool error onto an isError result.
func (s *server) call(ctx context.Context, id json.RawMessage, params json.RawMessage) []byte {
	var cp struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	// The params level itself is lenient: a client may add _meta and
	// other keys, only name and arguments matter.
	if err := json.Unmarshal(params, &cp); err != nil {
		return newRPCError(id, codeInvalidParams, "tools/call params: "+err.Error())
	}
	t := findTool(cp.Name)
	if t == nil {
		return newRPCError(id, codeInvalidParams, fmt.Sprintf("unknown tool %q", cp.Name))
	}
	args := newArgsFor(t.Name)
	// An absent or null arguments object behaves like an empty one, so
	// the tool's own validation reports what is missing.
	if len(cp.Arguments) > 0 && !bytes.Equal(bytes.TrimSpace(cp.Arguments), []byte("null")) {
		if err := strictArgs(cp.Arguments, args); err != nil {
			return newRPCError(id, codeInvalidParams, fmt.Sprintf("%s arguments: %s", t.Name, err))
		}
	}
	timeout := readTimeout
	if t.Name == "stop_stack" || t.Name == "remove_stack" {
		timeout = actionTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := s.runTool(tctx, t.Name, args)
	if err != nil {
		return response(id, callResult{
			Content: []contentItem{{Type: "text", Text: err.Error()}},
			IsError: true,
		})
	}
	return response(id, res)
}

// newArgsFor returns a fresh zero-valued args struct for the tool.
func newArgsFor(name string) any {
	switch name {
	case "stack_details":
		return &stackDetailsArgs{}
	case "stop_stack":
		return &nameArgs{}
	case "remove_stack":
		return &removeArgs{}
	default:
		return &struct{}{}
	}
}

// strictArgs decodes one JSON object strictly: no unknown keys, no
// trailing data, no non-object input.
func strictArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return errors.New("arguments must be an object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("arguments must be one JSON object")
	}
	return nil
}

type stackDetailsArgs struct {
	Name       string  `json:"name"`
	LogService string  `json:"log_service"`
	LogLines   float64 `json:"log_lines"`
}

type nameArgs struct {
	Name string `json:"name"`
}

type removeArgs struct {
	Name    string `json:"name"`
	Confirm string `json:"confirm"`
}

func (s *server) runTool(ctx context.Context, name string, args any) (callResult, error) {
	switch name {
	case "list_stacks":
		return s.listStacks(ctx)
	case "stack_details":
		a := args.(*stackDetailsArgs)
		if a.Name == "" {
			return callResult{}, errors.New("stack_details needs the stack name")
		}
		return s.stackDetails(ctx, a)
	case "stop_stack":
		a := args.(*nameArgs)
		if a.Name == "" {
			return callResult{}, errors.New("stop_stack needs the stack name")
		}
		return s.act(ctx, "stop", a.Name, "")
	case "remove_stack":
		ra := args.(*removeArgs)
		if ra.Name == "" {
			return callResult{}, errors.New("remove_stack needs the stack name")
		}
		return s.act(ctx, "remove", ra.Name, ra.Confirm)
	}
	return callResult{}, fmt.Errorf("unknown tool %q", name) // unreachable: call validated the name
}

// stackMap is the wire form of one view.Stack: next_at is omitted for a
// zero time, holders are never nil, and the times are UTC whatever zone
// the clock runs in.
func stackMap(st view.Stack) map[string]any {
	m := map[string]any{
		"name":               st.Name,
		"path":               st.Path,
		"state":              st.State,
		"next":               st.Next,
		"never_remove":       st.NeverRemove,
		"containers_running": st.Running,
		"containers_total":   st.Running + st.Stopped,
		"disk_bytes":         st.DiskBytes,
	}
	if !st.NextAt.IsZero() {
		m["next_at"] = st.NextAt.UTC().Format(time.RFC3339)
	}
	holders := make([]map[string]any, 0, len(st.Holders))
	for _, h := range st.Holders {
		holders = append(holders, map[string]any{
			"name":  h.Name,
			"until": h.Until.UTC().Format(time.RFC3339),
		})
	}
	m["holders"] = holders
	return m
}

func (s *server) listStacks(ctx context.Context) (callResult, error) {
	stacks, err := view.List(ctx, s.tools.Cli, s.tools.Store, s.tools.Cfg, s.now())
	if err != nil {
		return callResult{}, err
	}
	out := make([]map[string]any, 0, len(stacks))
	var b strings.Builder
	for i, st := range stacks {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(st.Name + ": " + st.State)
		if st.Next != "" {
			b.WriteString(", " + st.Next)
		}
		out = append(out, stackMap(st))
	}
	return callResult{
		Content:           []contentItem{{Type: "text", Text: b.String()}},
		StructuredContent: map[string]any{"stacks": out},
	}, nil
}

func (s *server) stackDetails(ctx context.Context, a *stackDetailsArgs) (callResult, error) {
	if a.LogLines < 0 || a.LogLines > 200 || a.LogLines != math.Trunc(a.LogLines) {
		return callResult{}, errors.New("log_lines must be a whole number between 0 and 200")
	}
	// log_lines omitted (0) means no logs, also when log_service is
	// given: only an explicit line count fetches logs.
	logService := a.LogService
	if a.LogLines == 0 {
		logService = ""
	}
	d, err := view.Details(ctx, s.tools.Cli, s.tools.Store, s.tools.Cfg, a.Name, logService, int(a.LogLines), s.now())
	if err != nil {
		return callResult{}, err
	}
	m := stackMap(d.Stack)
	conts := make([]map[string]any, 0, len(d.Containers))
	for _, c := range d.Containers {
		conts = append(conts, map[string]any{
			"service":      c.Service,
			"name":         c.Name,
			"state":        c.State,
			"status":       c.Status,
			"image":        c.Image,
			"memory_bytes": c.MemoryBytes,
			"kept":         c.Kept,
		})
	}
	m["containers"] = conts
	vols := make([]map[string]any, 0, len(d.Volumes))
	for _, v := range d.Volumes {
		vols = append(vols, map[string]any{
			"name":  v.Name,
			"bytes": v.Bytes,
			"kept":  v.Kept,
		})
	}
	m["volumes"] = vols
	// Every array is an array on the wire, never null: the output schema
	// says type array.
	m["removed_images"] = append([]string{}, d.RemovedImages...)
	m["kept_images"] = append([]string{}, d.KeptImages...)
	m["networks"] = append([]string{}, d.Networks...)
	m["start_command"] = d.StartCommand
	m["log_service"] = d.LogService
	logs := make([]map[string]any, 0, len(d.Logs))
	for _, l := range d.Logs {
		lm := map[string]any{"stream": l.Stream, "text": l.Text}
		if !l.Time.IsZero() {
			lm["time"] = l.Time.UTC().Format(time.RFC3339)
		}
		logs = append(logs, lm)
	}
	m["logs"] = logs
	var b strings.Builder
	b.WriteString(d.Name + ": " + d.State)
	if d.Next != "" {
		b.WriteString(", " + d.Next)
	}
	b.WriteString(", " + strconv.Itoa(len(d.Containers)) + " containers (" + strconv.Itoa(d.Running) + " running)")
	return callResult{
		Content:           []contentItem{{Type: "text", Text: b.String()}},
		StructuredContent: m,
	}, nil
}

// act runs one action through the serve process's control socket: it
// returns the result when serve answered ok and a tool error otherwise.
func (s *server) act(ctx context.Context, action, name, confirm string) (callResult, error) {
	// remove_stack refuses a mismatched confirm before any control call.
	if action == "remove" && confirm != name {
		return callResult{}, fmt.Errorf("refusing to remove %s: confirm must be exactly the stack name %q", name, name)
	}
	// Only managed stacks the view knows exist may be acted on, so an
	// unmanaged name or a name with nothing behind it never reaches the
	// control socket.
	if !reaper.Managed(name, s.tools.Cfg) {
		return callResult{}, fmt.Errorf("dev-lease does not manage a stack named %s", name)
	}
	stacks, err := view.List(ctx, s.tools.Cli, s.tools.Store, s.tools.Cfg, s.now())
	if err != nil {
		return callResult{}, err
	}
	known := false
	for _, st := range stacks {
		if st.Name == name {
			known = true
			break
		}
	}
	if !known {
		// A managed name the view does not list either has a lease record
		// with nothing on the daemon yet (extend ran before compose up) or
		// exists nowhere; the messages differ.
		leases, rerr := s.tools.Store.Read()
		if rerr != nil {
			return callResult{}, rerr
		}
		if _, has := leases[name]; has {
			if action == "stop" {
				return callResult{}, fmt.Errorf("nothing of %s is on the daemon; there is nothing to stop", name)
			}
			return callResult{}, fmt.Errorf("nothing of %s is on the daemon; there is nothing to remove", name)
		}
		return callResult{}, fmt.Errorf("dev-lease does not manage a stack named %s", name)
	}
	// The tool context's own deadline is the only limit; serve may take
	// a while to stop a big stack.
	limit := actionTimeout
	if dl, ok := ctx.Deadline(); ok {
		limit = time.Until(dl)
	}
	ans, err := control.Call(ctx, s.tools.ControlPath, control.Request{Action: action, Project: name, Confirm: confirm})
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
			return callResult{}, fmt.Errorf("dev-lease serve is not running (no control socket at %s); start it and try again", s.tools.ControlPath)
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
			return callResult{}, timeoutText(limit)
		default:
			// The deadline may have closed the connection first; decide
			// from the context.
			switch ctx.Err() {
			case context.DeadlineExceeded:
				return callResult{}, timeoutText(limit)
			case context.Canceled:
				return callResult{}, context.Canceled
			}
			return callResult{}, err
		}
	}
	if !ans.OK {
		// A stop whose container listing failed carries the project in
		// Failed: give it the stop-specific text, the refusal codes get
		// their sentences.
		if action == "stop" && ans.Report != nil && slices.Contains(ans.Report.Failed, name) {
			return callResult{}, errors.New("the stop of " + name + " did not finish (the container listing failed); the next sweep retries it, or revives the stack if its containers run again")
		}
		return callResult{}, errors.New(mappedRefusal(name, ans))
	}
	res := callResult{StructuredContent: map[string]any{"name": name}}
	var b strings.Builder
	if action == "stop" {
		// Stop results get their own texts: a listing failure means the
		// stop did not finish, failed container stops are named.
		if ans.Report != nil && slices.Contains(ans.Report.Failed, name) {
			b.WriteString("the stop of " + name + " did not finish (the container listing failed); the next sweep retries it, or revives the stack if its containers run again")
		} else {
			b.WriteString("stopped " + name)
			if r := ans.Report; r != nil {
				if len(r.Failed) > 0 {
					b.WriteString("; could not stop: " + strings.Join(r.Failed, ", "))
				}
				if len(r.Skipped) > 0 {
					b.WriteString("; kept: " + strings.Join(r.Skipped, ", "))
				}
			}
			b.WriteString("; nothing was deleted")
			if d, err := view.Details(ctx, s.tools.Cli, s.tools.Store, s.tools.Cfg, name, "", 0, s.now()); err == nil && d.StartCommand != "" {
				b.WriteString("\nstart it again with: " + d.StartCommand)
			}
		}
	} else {
		b.WriteString("removed " + name)
		if r := ans.Report; r != nil {
			if len(r.Skipped) > 0 {
				b.WriteString("; kept shared objects: " + strings.Join(r.Skipped, ", "))
			}
			if len(r.Failed) > 0 {
				b.WriteString("; failed to remove: " + strings.Join(r.Failed, ", ") + " (see serve's log)")
			}
		}
	}
	res.Content = []contentItem{{Type: "text", Text: b.String()}}
	return res, nil
}

// timeoutText is the tool error of an action that outlived the tool
// context.
func timeoutText(limit time.Duration) error {
	return fmt.Errorf("the action did not answer within %s; serve may still finish it, check list_stacks", limit.Round(time.Second))
}

// mappedRefusal turns a serve refusal into a sentence the MCP client can
// show as-is; the answer's message passes through when it already reads
// as a sentence.
func mappedRefusal(name string, ans control.Answer) string {
	switch ans.Code {
	case control.CodeNeverRemove:
		return name + " is marked never remove; remove it from the dashboard if you mean it"
	case control.CodeBusy:
		return name + " is being stopped or removed right now; try again in a minute"
	case control.CodeNoStack, control.CodeNotManaged:
		return "dev-lease does not manage a stack named " + name
	case control.CodeShuttingDown:
		return "dev-lease serve is shutting down; try again when it is back"
	default:
		return ans.Error
	}
}
