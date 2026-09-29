package mcp

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeProxy is the deterministic stand-in for a running millivolt. It records
// every request it receives and answers from a per-path canned table, so a test
// can assert the exact method, path, query and Authorization header a tool
// produced, and can map a canned response onto the tool's output.
type fakeProxy struct {
	server *httptest.Server
	mu     sync.Mutex
	// seen records every request in arrival order.
	seen []seenRequest
	// routes maps "METHOD path" (path only, query excluded) to a response.
	routes map[string]cannedResponse
}

type seenRequest struct {
	Method string
	Path   string
	Query  url.Values
	Auth   string
	Body   string
}

type cannedResponse struct {
	Status      int
	ContentType string
	Body        string
	Headers     map[string]string
}

func newFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()
	proxy := &fakeProxy{routes: map[string]cannedResponse{}}
	proxy.server = httptest.NewServer(http.HandlerFunc(proxy.serve))
	t.Cleanup(proxy.server.Close)
	return proxy
}

// respond registers a canned answer for one method and path.
func (f *fakeProxy) respond(method, path string, response cannedResponse) *fakeProxy {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[method+" "+path] = response
	return f
}

// json registers a 200 JSON answer.
func (f *fakeProxy) json(method, path string, body string) *fakeProxy {
	return f.respond(method, path, cannedResponse{Status: http.StatusOK, ContentType: "application/json", Body: body})
}

// error registers a failure with the proxy's flat error body. contentType is
// explicit because the operator routes are inconsistent: some answer
// application/json and some text/plain with the same JSON body.
func (f *fakeProxy) fail(method, path string, status int, message, contentType string) *fakeProxy {
	if contentType == "" {
		contentType = "application/json"
	}
	return f.respond(method, path, cannedResponse{
		Status:      status,
		ContentType: contentType,
		Body:        `{"error":` + quote(message) + `}`,
	})
}

func (f *fakeProxy) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.seen = append(f.seen, seenRequest{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(),
		Auth: r.Header.Get("Authorization"), Body: string(body),
	})
	response, ok := f.routes[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no canned route"}`))
		return
	}
	for key, value := range response.Headers {
		w.Header().Set(key, value)
	}
	contentType := response.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(response.Status)
	_, _ = w.Write([]byte(response.Body))
}

// requests returns the recorded requests.
func (f *fakeProxy) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.seen...)
}

// last returns the most recent request, failing the test when there was none.
func (f *fakeProxy) last(t *testing.T) seenRequest {
	t.Helper()
	seen := f.requests()
	if len(seen) == 0 {
		t.Fatal("no request reached the fake proxy")
	}
	return seen[len(seen)-1]
}

// requestsFor returns every recorded request for one method and path.
func (f *fakeProxy) requestsFor(method, path string) []seenRequest {
	var out []seenRequest
	for _, request := range f.requests() {
		if request.Method == method && request.Path == path {
			out = append(out, request)
		}
	}
	return out
}

func (f *fakeProxy) origin() string { return f.server.URL }

func quote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// testToken is a syntactically valid operator credential for the fake proxy.
// Tests assert it never appears in any tool output. It is deliberately
// high-entropy: a word-like fixture shares 8-byte runs with ordinary error
// prose ("operator" inside test-operator-credential-value), and the redaction
// treats every such run as the credential, so a word-like fixture would make
// assertions about readable proxy messages test the fixture, not the behavior.
const testToken = "9f2c7a4e1b8d5f30c6a9e2b7d4f81c53"

// newTestService builds a service pointed at the fake proxy.
func newTestService(t *testing.T, proxy *fakeProxy, limits Limits) *Service {
	t.Helper()
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	service, err := NewService(proxy.origin(), testToken, limits)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// decodeBody parses a recorded JSON request body.
func decodeBody(t *testing.T, request seenRequest) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(request.Body), &document); err != nil {
		t.Fatalf("request body is not a JSON object: %v (%q)", err, request.Body)
	}
	return document
}

// assertAuth pins the credential presentation: Bearer on every request, and
// never a session cookie.
func assertAuth(t *testing.T, request seenRequest) {
	t.Helper()
	if want := "Bearer " + testToken; request.Auth != want {
		t.Fatalf("Authorization = %q, want %q", request.Auth, want)
	}
}

// assertQuery pins one query parameter, including its absence.
func assertQuery(t *testing.T, request seenRequest, key, want string) {
	t.Helper()
	if got := request.Query.Get(key); got != want {
		t.Fatalf("query %s = %q, want %q (full query %q)", key, got, want, request.Query.Encode())
	}
}

// assertNoQuery pins that a parameter was not sent.
func assertNoQuery(t *testing.T, request seenRequest, key string) {
	t.Helper()
	if _, present := request.Query[key]; present {
		t.Fatalf("query %s must not be sent (full query %q)", key, request.Query.Encode())
	}
}

// assertNoToken fails when the credential leaked into any text a model could
// read: a request URL, a request body, or an error message.
func assertNoToken(t *testing.T, where, text string) {
	t.Helper()
	if strings.Contains(text, testToken) {
		t.Fatalf("operator token leaked into %s", where)
	}
}

// sink records anything a redirect target receives. It exists so the redirect
// guard can assert a negative - that nothing was replayed - rather than only
// that an error came back.
type sink struct {
	mu       sync.Mutex
	seen     []seenRequest
	listener net.Listener
	server   *httptest.Server
}

func newSink(t *testing.T) *sink {
	t.Helper()
	s := &sink{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, seenRequest{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(),
			Auth: r.Header.Get("Authorization"), Body: string(body),
		})
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *sink) url() string { return s.server.URL }

func (s *sink) requests() []seenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seenRequest(nil), s.seen...)
}
