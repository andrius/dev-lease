package mcp

import "testing"

// Every id that is neither a string nor an integer makes the request
// invalid: exponents, fractions, booleans, objects, arrays and null are
// each answered with -32600 and id null.
func TestNonStringNonIntegerIDsAreInvalidRequests(t *testing.T) {
	s := startSession(t, Tools{Version: "test"})
	defer s.close(t)
	for _, id := range []string{
		`1E3`, `1e3`, `1.5`, `true`, `{}`, `[]`, `null`,
	} {
		t.Run("id "+id, func(t *testing.T) {
			s.send(t, `{"jsonrpc":"2.0","id":`+id+`,"method":"ping"}`)
			m := s.read(t)
			if code := rpcErrorCode(t, m); code != -32600 {
				t.Errorf("error code = %d, want -32600", code)
			}
			if m["id"] != nil {
				t.Errorf("id = %v, want null", m["id"])
			}
		})
	}
	// Controls: a string id and an integer id are valid and echoed.
	s.send(t, `{"jsonrpc":"2.0","id":"abc","method":"ping"}`)
	if m := s.read(t); m["id"] != "abc" || m["error"] != nil {
		t.Errorf("string id answer = %v, want the ping result", m)
	}
	s.send(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if m := s.read(t); m["id"] != float64(1) || m["error"] != nil {
		t.Errorf("integer id answer = %v, want the ping result", m)
	}
}
