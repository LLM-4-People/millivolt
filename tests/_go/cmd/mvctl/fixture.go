package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

const testToken = "0123456789abcdef"

// runCLI executes one invocation with the operator credential in the
// environment, exactly as an operator supplies it. Flags after the command are
// parsed by the command's own set, so tests exercise the real dispatch path.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv(mcp.ProxyTokenEnv, testToken)
	var stdout, stderr bytes.Buffer
	code := runWith(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

type proxyCall struct {
	Method string
	Path   string
	Query  string
	Body   string
	Auth   string
}

type proxyFixture struct {
	t      *testing.T
	server *httptest.Server
	routes map[string]http.HandlerFunc
	mu     sync.Mutex
	calls  []proxyCall
}

func newProxy(t *testing.T, routes map[string]http.HandlerFunc) *proxyFixture {
	t.Helper()
	fixture := &proxyFixture{t: t, routes: routes}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		fixture.mu.Lock()
		fixture.calls = append(fixture.calls, proxyCall{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Body: string(body), Auth: r.Header.Get("Authorization"),
		})
		fixture.mu.Unlock()
		if r.URL.Path != "/healthz" && r.Header.Get("Authorization") != "Bearer "+testToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"operator token rejected"}`)
			return
		}
		handler, ok := fixture.routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		handler(w, r)
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *proxyFixture) url() string { return f.server.URL }

func (f *proxyFixture) callsFor(path string) []proxyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []proxyCall
	for _, call := range f.calls {
		if call.Path == path {
			out = append(out, call)
		}
	}
	return out
}

func (f *proxyFixture) callCount(path string) int { return len(f.callsFor(path)) }

func jsonHandler(t *testing.T, payload string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	}
}

func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("decode got body %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("decode want body %q: %v", want, err)
	}
	gotJSON, _ := json.Marshal(gotValue)
	wantJSON, _ := json.Marshal(wantValue)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("body = %s, want %s", gotJSON, wantJSON)
	}
}
