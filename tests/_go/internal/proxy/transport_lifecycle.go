package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type lifecycleSignalWriter struct {
	*httptest.ResponseRecorder
	once    sync.Once
	started chan struct{}
}

func (w *lifecycleSignalWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(b)
	if bytes.Contains(b, []byte(`"object":"chat.completion.chunk"`)) {
		w.once.Do(func() { close(w.started) })
	}
	return n, err
}

func TestActiveContinuationCancelsAfterHeaders(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		readFrame(t, r.Body)
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}), &http2.Server{}))
	defer up.Close()
	defer close(release)
	p := New(config.Default(), nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := httptest.NewRequest("POST", "http://proxy/v1/chat/completions", strings.NewReader(`{"model":"model-neutral","stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	r.Header.Set(hdrBaseURL, up.URL)
	r.Header.Set(hdrFormat, "cursor")
	w := &lifecycleSignalWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{})}
	done := make(chan struct{})
	go func() { p.ServeHTTP(w, r); close(done) }()
	select {
	case <-w.started:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled active turn retained its idle upstream")
	}
}

func TestDebugTimerCannotRemoveExtendedSession(t *testing.T) {
	p := New(config.Default(), nil)
	defer p.applyDebug(nil)
	old := time.Now().Add(time.Minute)
	p.applyDebug([]persistedDebug{{ID: "session-neutral", Clients: []string{"client-neutral"}, Until: old.Add(time.Hour)}})
	p.removeDebug("session-neutral", old)
	p.debug.mu.Lock()
	defer p.debug.mu.Unlock()
	if len(p.debug.sessions) != 1 {
		t.Fatal("old expiry removed an extended session")
	}
}

func TestContinuationScopeOwnsParkedIdentity(t *testing.T) {
	p := New(config.Default(), nil)
	base := target{baseURL: "https://old.example", provider: "old.example", authHeader: "Authorization", authPrefix: "Bearer "}
	identity := p.cursorScopeFor(&base, "key-a", "raw-model-a", "client-a")
	changed := func(f func(*target)) cursorScope {
		v := base
		f(&v)
		return p.cursorScopeFor(&v, "key-a", "raw-model-a", "client-a")
	}
	variants := map[string]cursorScope{
		"base":        changed(func(v *target) { v.baseURL = "https://new.example" }),
		"path":        changed(func(v *target) { v.path = "/different/run" }),
		"auth header": changed(func(v *target) { v.authHeader = "X-Credential" }),
		"auth prefix": changed(func(v *target) { v.authPrefix = "Token " }),
		"key":         p.cursorScopeFor(&base, "key-b", "raw-model-a", "client-a"),
		"raw model":   p.cursorScopeFor(&base, "key-a", "raw-model-b", "client-a"),
		"client":      p.cursorScopeFor(&base, "key-a", "raw-model-a", "client-b"),
	}
	run := fakeParkedRun(t)
	defer run.Close()
	if !p.cursorRuns.parkWith(identity, run, []string{"call-a"}) {
		t.Fatal("park failed")
	}
	for name, scope := range variants {
		if scope == identity {
			t.Errorf("%s did not distinguish scope", name)
		}
		if got, _ := p.cursorRuns.findByToolTail(scope, []string{"call-a"}); got != nil {
			t.Errorf("%s consumed another scope", name)
		}
	}
	if got, _ := p.cursorRuns.findByToolTail(identity, []string{"historic", "call-a"}); got != run {
		t.Fatal("matching tail did not resume")
	}
}

func TestParkCollisionAndScopeIndexesRemainIndependent(t *testing.T) {
	s := newCursorRunStore(time.Hour)
	a, b := fakeParkedRun(t), fakeParkedRun(t)
	defer a.Close()
	defer b.Close()
	scopeA, scopeB := cursorScope{1}, cursorScope{2}
	if !s.parkWith(scopeA, a, []string{"shared", "a"}) {
		t.Fatal("first park failed")
	}
	if s.parkWith(scopeA, b, []string{"shared", "b"}) {
		t.Fatal("duplicate index replaced owner")
	}
	if s.parkWith(scopeB, b, []string{"same", "same"}) {
		t.Fatal("duplicate IDs in one park accepted")
	}
	if !s.parkWith(scopeB, b, []string{"shared", "b"}) {
		t.Fatal("independent scope rejected")
	}
	s.drop(a)
	if got, _ := s.findByToolTail(scopeB, []string{"b"}); got != b {
		t.Fatal("dropping other scope damaged index")
	}
}

func TestParkExpiryDoesNotRequireTraffic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newCursorRunStore(time.Second)
		run := fakeParkedRun(t)
		defer run.Close()
		if !s.parkWith(cursorScope{}, run, []string{"call-a"}) {
			t.Fatal("park failed")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if !run.Closed() {
			t.Fatal("idle expiry left run open")
		}
		if len(s.entries) != 0 || s.timer != nil {
			t.Fatal("idle expiry retained indexes/timer")
		}
	})
}

func TestResponseSecretsExcludedFromRetainedMetricsOnly(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=synthetic-secret")
		io.WriteString(w, realBody)
	}))
	defer up.Close()
	buf := metrics.NewBuffer(5)
	p := New(config.Default(), buf)
	r := httptest.NewRequest("POST", "http://proxy/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	r.Header.Set(hdrBaseURL, up.URL)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	// An upstream cookie must never enter the browser's millivolt-origin jar,
	// where it could shadow operator-plane state (operator gate boundary).
	if w.Header().Get("Set-Cookie") != "" {
		t.Fatal("upstream Set-Cookie was forwarded to the client")
	}
	raw, err := json.Marshal(waitForRecord(t, buf, 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("synthetic-secret")) {
		t.Fatal("retained record contains response credential")
	}
}

func TestCustomAuthIsRedactedFromDebugCapture(t *testing.T) {
	p := New(config.Default(), nil)
	p.applyDebug([]persistedDebug{{ID: "session-neutral", Clients: []string{"client-neutral"}}})
	defer p.applyDebug(nil)
	r := httptest.NewRequest("POST", "http://proxy/", nil)
	r.Header.Set("X-Credential", "synthetic-secret")
	tap := p.beginDebugTap(r, &target{authHeader: "X-Credential"}, &metrics.Record{Client: "client-neutral"}, nil)
	raw, err := json.Marshal(tap.reqHdr)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("synthetic-secret")) {
		t.Fatal("configured auth value retained")
	}
	if r.Header.Get("X-Credential") != "synthetic-secret" {
		t.Fatal("capture mutated request")
	}
}

func TestHeaderAuthorityRejectsDuplicateNames(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderOverride{"old.example": {Headers: map[string]string{"X-Identity": "one", "x-identity": "two"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("case-duplicate configured headers accepted")
	}
	for _, in := range []string{`{"X-Identity":["one"],"x-identity":["two"]}`, `{"X-Identity":["one"],"X-Identity":["two"]}`, `null`, `{"X:Invalid":["one"]}`} {
		r := httptest.NewRequest("POST", "http://proxy/", nil)
		r.Header.Set(hdrBaseURL, "https://old.example")
		r.Header.Set(hdrHeaders, in)
		if _, err := resolveTarget(r, config.Default()); err == nil {
			t.Errorf("invalid injected headers accepted: %s", in)
		}
	}
}

type lifecycleReadError struct{}

func (lifecycleReadError) Read([]byte) (int, error) { return 0, context.Canceled }
func TestTranslatedReadCancellationKeepsClientStatus(t *testing.T) {
	p := New(config.Default(), nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rec := &metrics.Record{StatusCode: 200}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "http://proxy/", nil).WithContext(ctx)
	resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(lifecycleReadError{})}
	p.serveNonStreaming(ctx, w, resp, r, &target{format: "anthropic"}, "", nil, rec, "", scheduler.WaiterHooks{})
	if rec.StatusCode != metrics.StatusClientClosedRequest || !rec.ClientDisconnected || rec.ErrorType != "" {
		t.Fatalf("cancellation changed: %+v", rec)
	}
}

func TestRedirectResponsesStayUnchanged(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, stream := range []bool{false, true} {
			for _, body := range []string{"", "redirect body"} {
				t.Run(fmt.Sprintf("%d/stream=%t/body=%t", status, stream, body != ""), func(t *testing.T) {
					var calls atomic.Int32
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						w.Header().Set("Location", "http://new.example/next")
						w.WriteHeader(status)
						io.WriteString(w, body)
					}))
					defer up.Close()
					buf := metrics.NewBuffer(5)
					p := New(config.Default(), buf)
					r := httptest.NewRequest("POST", "http://proxy/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"neutral","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream)))
					r.Header.Set(hdrBaseURL, up.URL)
					w := httptest.NewRecorder()
					p.ServeHTTP(w, r)
					if w.Code != status || w.Body.String() != body || w.Header().Get("Location") != "http://new.example/next" || calls.Load() != 1 {
						t.Fatalf("redirect changed: status=%d body=%q calls=%d", w.Code, w.Body.String(), calls.Load())
					}
					if rec := waitForRecord(t, buf, 1)[0]; rec.StatusCode != status {
						t.Fatalf("stored status=%d", rec.StatusCode)
					}
				})
			}
		}
	}
}
