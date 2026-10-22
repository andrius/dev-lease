package fakedocker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
)

func TestServerVersionAndLists(t *testing.T) {
	s := New(t)
	s.Add("container", "c1", "web", "myapp")
	s.Add("container", "c2", "db", "other")

	hc := client(t, s.Socket)

	body, status := get(t, hc, "/version")
	if status != 200 {
		t.Fatalf("GET /version = %d", status)
	}
	var v struct{ ApiVersion string }
	if err := json.Unmarshal(body, &v); err != nil || v.ApiVersion != apiVersion {
		t.Fatalf("GET /version body = %s, err %v", body, err)
	}

	q := url.QueryEscape(`{"label":["com.docker.compose.project=myapp"]}`)
	body, status = get(t, hc, "/v1.47/containers/json?filters="+q)
	if status != 200 {
		t.Fatalf("GET containers = %d", status)
	}
	var conts []struct {
		Id string `json:"Id"`
	}
	if err := json.Unmarshal(body, &conts); err != nil {
		t.Fatal(err)
	}
	if len(conts) != 1 || conts[0].Id != "c1" {
		t.Fatalf("filtered containers = %+v, want only c1", conts)
	}
}

func TestServerRefusesOneObject(t *testing.T) {
	s := New(t)
	s.Add("volume", "v1", "v1", "myapp")
	s.Refuse("volume", "v1", 409)

	hc := client(t, s.Socket)
	_, status := del(t, hc, "/v1.47/volumes/v1")
	if status != 409 {
		t.Fatalf("DELETE volume = %d, want 409", status)
	}
	if !s.Has("volume", "v1") {
		t.Fatal("refused volume was removed")
	}
	if len(s.Calls()) != 1 {
		t.Fatalf("Calls() = %d entries, want 1", len(s.Calls()))
	}

	s.Unrefuse("volume", "v1")
	_, status = del(t, hc, "/v1.47/volumes/v1")
	if status != 204 {
		t.Fatalf("DELETE volume after unrefuse = %d, want 204", status)
	}
	if s.Has("volume", "v1") {
		t.Fatal("volume still present after successful delete")
	}
}

func TestServerMissingObjectIs404(t *testing.T) {
	s := New(t)
	hc := client(t, s.Socket)
	_, status := del(t, hc, "/v1.47/volumes/ghost")
	if status != 404 {
		t.Fatalf("DELETE missing volume = %d, want 404", status)
	}
}

// The fake honours the logs tail parameter: GET with tail=200 of 300
// stored lines answers exactly the last 200, in order.
func TestContainerLogsTailRing(t *testing.T) {
	s := New(t)
	s.Add("container", "c1", "web", "myapp")
	var raw []byte
	for i := 1; i <= 300; i++ {
		raw = append(raw, muxFrame(1, []byte(fmt.Sprintf("2026-10-20T12:00:00.000000000Z line %03d\n", i)))...)
	}
	s.SetLogs("c1", raw)
	hc := client(t, s.Socket)
	body, status := get(t, hc, "/v1.47/containers/c1/logs?stdout=1&stderr=1&timestamps=1&tail=200")
	if status != 200 {
		t.Fatalf("GET logs = %d", status)
	}
	var lines [][]byte
	for rest := body; len(rest) >= 8; {
		size := int(binary.BigEndian.Uint32(rest[4:8]))
		if len(rest) < 8+size {
			t.Fatalf("logs body ends inside a frame: %d bytes left", len(rest))
		}
		lines = append(lines, rest[8:8+size])
		rest = rest[8+size:]
	}
	if len(lines) != 200 {
		t.Fatalf("tail=200 of 300 lines answered %d lines", len(lines))
	}
	for i, l := range lines {
		want := fmt.Sprintf("2026-10-20T12:00:00.000000000Z line %03d\n", 101+i)
		if string(l) != want {
			t.Fatalf("line %d = %q, want %q (exactly the last 200, in order)", i, l, want)
		}
	}
}

func muxFrame(stream byte, p []byte) []byte {
	f := make([]byte, 8+len(p))
	f[0] = stream
	binary.BigEndian.PutUint32(f[4:8], uint32(len(p)))
	copy(f[8:], p)
	return f
}

func client(t *testing.T, socket string) *http.Client {
	t.Helper()
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
}

func get(t *testing.T, hc *http.Client, path string) ([]byte, int) {
	t.Helper()
	resp, err := hc.Get("http://docker" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body, resp.StatusCode
}

func del(t *testing.T, hc *http.Client, path string) ([]byte, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, "http://docker"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body, resp.StatusCode
}
