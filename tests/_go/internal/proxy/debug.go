package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

func TestHandleDebugRequiresEnabledField(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	req := httptest.NewRequest(http.MethodPost, "/admin/debug", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	p.HandleDebug(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing enabled field", rr.Code)
	}
}

func TestDebugSnapshotIncludesObserverNamesForKnownAndScopedModels(t *testing.T) {
	s := New(config.Default(), metrics.Noop{})
	s.debug.sessions = []persistedDebug{{ID: "scope", Models: []string{"ScopedModel"}}}
	s.ModelObserver = func(names []string) any {
		out := map[string]string{}
		for _, name := range names {
			out[name] = strings.ToLower(name)
		}
		return map[string]any{"revision": "fixture", "names": out}
	}
	p := s.DebugSnapshot()
	observer := p["model_canon"].(map[string]any)
	if observer["names"].(map[string]string)["ScopedModel"] != "scopedmodel" {
		t.Fatalf("session model missing: %v", observer)
	}
	for _, name := range p["known_models"].([]string) {
		if _, ok := observer["names"].(map[string]string)[name]; !ok {
			t.Fatalf("known model missing: %q", name)
		}
	}
}

func TestHandleDebugRejectsAmbiguousJSON(t *testing.T) {
	s := New(config.Default(), metrics.Noop{})
	for _, body := range []string{
		`{"enabled":false,"enabld":true}`,
		`{"enabled":true,"enabled":false}`,
		`{"enabled":true,"ENABLED":false}`,
		`{"enabled":false}{"enabled":true}`,
		`null`,
	} {
		w := httptest.NewRecorder()
		s.HandleDebug(w, httptest.NewRequest(http.MethodPost, "/admin/debug", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400", body, w.Code)
		}
	}
}

func TestScopedOperatorIDsNeverWidenToStopAll(t *testing.T) {
	for _, tc := range []struct {
		name, flag, collection string
		handler                func(*Server) http.HandlerFunc
	}{
		{"pause", "paused", "holds", func(s *Server) http.HandlerFunc { return s.HandlePause }},
		{"debug", "enabled", "sessions", func(s *Server) http.HandlerFunc { return s.HandleDebug }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.handler(New(config.Default(), metrics.Noop{}))
			post := func(body string) *httptest.ResponseRecorder {
				rr := httptest.NewRecorder()
				h(rr, httptest.NewRequest(http.MethodPost, "/admin/"+tc.name, strings.NewReader(body)))
				return rr
			}
			ids := func() []struct{ ID string } {
				rr := httptest.NewRecorder()
				h(rr, httptest.NewRequest(http.MethodGet, "/admin/"+tc.name, nil))
				var doc map[string]json.RawMessage
				if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
					t.Fatal(err)
				}
				var scopes []struct{ ID string }
				if err := json.Unmarshal(doc[tc.collection], &scopes); err != nil {
					t.Fatal(err)
				}
				return scopes
			}
			for _, client := range []string{"client-a", "client-b"} {
				if rr := post(`{"` + tc.flag + `":true,"clients":["` + client + `"]}`); rr.Code != 200 {
					t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
				}
			}
			for _, rawID := range []string{`null`, `""`, `" \t "`, `0`, `[]`, `{}`} {
				for _, enabled := range []string{"false", "true"} {
					rr := post(`{"` + tc.flag + `":` + enabled + `,"id":` + rawID + `}`)
					if rr.Code != 400 || len(ids()) != 2 {
						t.Fatalf("explicit id=%s state=%s widened scope: %d %s", rawID, enabled, rr.Code, rr.Body.String())
					}
				}
			}
			first := ids()[0].ID
			if rr := post(`{"` + tc.flag + `":false,"id":"` + first + `"}`); rr.Code != 200 || len(ids()) != 1 {
				t.Fatalf("valid scoped stop: %d %s", rr.Code, rr.Body.String())
			}
			if rr := post(`{"` + tc.flag + `":false}`); rr.Code != 200 || len(ids()) != 0 {
				t.Fatalf("intentional omitted-id stop all: %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestHandleDebugGetAndPost(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})

	get := httptest.NewRecorder()
	p.HandleDebug(get, httptest.NewRequest(http.MethodGet, "/admin/debug", nil))
	if get.Code != 200 {
		t.Fatalf("GET status = %d", get.Code)
	}
	var st map[string]any
	if err := json.Unmarshal(get.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st["enabled"] != false {
		t.Fatalf("GET enabled = %v, want false", st["enabled"])
	}

	post := httptest.NewRecorder()
	p.HandleDebug(post, httptest.NewRequest(http.MethodPost, "/admin/debug",
		strings.NewReader(`{"enabled":true,"clients":["client-a"]}`)))
	if post.Code != 200 {
		t.Fatalf("POST status = %d body %s", post.Code, post.Body.Bytes())
	}
	if err := json.Unmarshal(post.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st["enabled"] != true {
		t.Fatalf("POST enabled = %v, want true", st["enabled"])
	}
}

func TestHandleDebugEmptyScope400(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandleDebug(rr, httptest.NewRequest(http.MethodPost, "/admin/debug",
		strings.NewReader(`{"enabled":true,"clients":[]}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body %s", rr.Code, rr.Body.Bytes())
	}
}

func TestHandleDebugOverlap409(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandleDebug(rr, httptest.NewRequest(http.MethodPost, "/admin/debug",
		strings.NewReader(`{"enabled":true,"clients":["client-a"]}`)))
	if rr.Code != 200 {
		t.Fatalf("first = %d %s", rr.Code, rr.Body.Bytes())
	}
	rr = httptest.NewRecorder()
	p.HandleDebug(rr, httptest.NewRequest(http.MethodPost, "/admin/debug",
		strings.NewReader(`{"enabled":true,"providers":["alpha.example"]}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("overlap status = %d, want 409 body %s", rr.Code, rr.Body.Bytes())
	}
}

func TestHandleDebugTwoNonOverlapping(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	for _, body := range []string{
		`{"enabled":true,"clients":["client-a"],"providers":["alpha.example"]}`,
		`{"enabled":true,"clients":["client-a"],"providers":["beta.example"]}`,
	} {
		rr := httptest.NewRecorder()
		p.HandleDebug(rr, httptest.NewRequest(http.MethodPost, "/admin/debug", strings.NewReader(body)))
		if rr.Code != 200 {
			t.Fatalf("status = %d body %s", rr.Code, rr.Body.Bytes())
		}
	}
	if len(p.debug.sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(p.debug.sessions))
	}
}

func TestDebugMatchAND(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandleDebug(rr, httptest.NewRequest(http.MethodPost, "/admin/debug",
		strings.NewReader(`{"enabled":true,"clients":["client-a"],"providers":["alpha.example"],"models":["m1"]}`)))
	if rr.Code != 200 {
		t.Fatalf("status = %d %s", rr.Code, rr.Body.Bytes())
	}
	if p.matchDebug("client-a", "alpha.example", "m1") == nil {
		t.Fatal("AND match missed")
	}
	if p.matchDebug("client-a", "alpha.example", "other") != nil {
		t.Fatal("model mismatch still matched")
	}
	if p.matchDebug("other", "alpha.example", "m1") != nil {
		t.Fatal("client mismatch still matched")
	}
}

func TestKnownModelsCanonicalizesFusedIds(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	p.noteModel("claude-4.5-sonnet-thinking")
	p.noteModel("claude-sonnet-4-5")
	p.noteModel(" grok-4.6-fast ")
	p.noteModel("grok-4.6")
	got := p.knownModels()
	if len(got) != 2 {
		t.Fatalf("known models = %v, want 2 canonical ids", got)
	}
	want := map[string]bool{"claude-sonnet-4-5": true, "grok-4.6": true}
	for _, m := range got {
		if !want[m] {
			t.Errorf("unexpected model %q", m)
		}
	}
}

func TestDebugMatchCanonicalModel(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandleDebug(rr, httptest.NewRequest(http.MethodPost, "/admin/debug",
		strings.NewReader(`{"enabled":true,"models":["claude-4.5-sonnet-thinking"]}`)))
	if rr.Code != 200 {
		t.Fatalf("status = %d %s", rr.Code, rr.Body.Bytes())
	}
	st := map[string]any{}
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	sessions, _ := st["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %v", st["sessions"])
	}
	sess := sessions[0].(map[string]any)
	models, _ := sess["models"].([]any)
	if len(models) != 1 || models[0] != "claude-sonnet-4-5" {
		t.Fatalf("stored models = %v, want canonical claude-sonnet-4-5", models)
	}
	if p.matchDebug("anyone", "any", "claude-sonnet-4-5") == nil {
		t.Fatal("canonical id should match the fused session")
	}
	if p.matchDebug("anyone", "any", "claude-4.5-sonnet-thinking") == nil {
		t.Fatal("fused id should still match after canonical storage")
	}
}

func TestDebugSnapshotMatchesGET(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	get := httptest.NewRecorder()
	p.HandleDebug(get, httptest.NewRequest(http.MethodGet, "/admin/debug", nil))
	snap, err := json.Marshal(p.DebugSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if get.Body.String() != string(snap) {
		t.Fatalf("GET %s\nsnap %s", get.Body.Bytes(), snap)
	}
}

func TestDebugPersistsAndRestores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debug.db")
	d := config.Default()
	store, err := storage.Open(path, storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: d.StorageFlushInterval,
		QueryTimeout:  d.StorageQueryTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}

	p := New(d, metrics.Noop{})
	p.AttachPausePersist(store)
	rr := httptest.NewRecorder()
	p.HandleDebug(rr, httptest.NewRequest(http.MethodPost, "/admin/debug",
		strings.NewReader(`{"enabled":true,"clients":["client-a"],"duration":"1h"}`)))
	if rr.Code != 200 {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.Bytes())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := storage.Open(path, storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: d.StorageFlushInterval,
		QueryTimeout:  d.StorageQueryTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	p2 := New(d, metrics.Noop{})
	p2.AttachPausePersist(store2)
	if p2.matchDebug("client-a", "any", "any") == nil {
		t.Fatal("restored session did not match client-a")
	}
	if p2.matchDebug("other", "any", "any") != nil {
		t.Fatal("restored session leaked to other clients")
	}
	if len(p2.debug.sessions) != 1 || p2.debug.sessions[0].Duration != "1h" {
		t.Fatalf("restored sessions = %+v, want duration 1h", p2.debug.sessions)
	}
}

func TestDebugRequestPublishedAndCaptured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debug-cap.db")
	d := config.Default()
	store, err := storage.Open(path, storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: d.StorageFlushInterval,
		QueryTimeout:  d.StorageQueryTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	spy := &liveSpy{}
	p := New(d, spy)
	p.AttachPausePersist(store)
	rr := httptest.NewRecorder()
	p.HandleDebug(rr, httptest.NewRequest(http.MethodPost, "/admin/debug",
		strings.NewReader(`{"enabled":true,"clients":["client-a"]}`)))
	if rr.Code != 200 {
		t.Fatalf("status = %d %s", rr.Code, rr.Body.Bytes())
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got == "" {
			t.Error("upstream missing Authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"hello debug"}}]}`))
	}))
	defer upstream.Close()

	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"secret prompt"}]}`))
	req.Header.Set("Authorization", "Bearer sk-secret")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Client", "client-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	var rec *metrics.Record
	for time.Now().Before(deadline) {
		if rec = spy.last.Load(); rec != nil && rec.Debug && rec.StatusCode == 200 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rec == nil || !rec.Debug {
		t.Fatal("live record never published with debug=true")
	}

	got := httptest.NewRecorder()
	p.HandleDebugCapture(got, httptest.NewRequest(http.MethodGet, "/metrics/debug?id="+rec.ID, nil))
	if got.Code != 200 {
		t.Fatalf("capture status = %d body %s", got.Code, got.Body.Bytes())
	}
	raw := got.Body.String()
	if strings.Contains(raw, "sk-secret") {
		t.Fatal("capture leaked the raw API key")
	}
	if !strings.Contains(raw, "[REDACTED]") {
		t.Fatal("capture missing redacted Authorization")
	}
	if !strings.Contains(raw, "secret prompt") {
		t.Fatal("capture missing request body content")
	}
	if !strings.Contains(raw, "hello debug") {
		t.Fatal("capture missing response body")
	}
}

func TestRedactHeaderName(t *testing.T) {
	if !redactHeaderName("Authorization", "") || !redactHeaderName("X-Api-Key", "") || !redactHeaderName("Cookie", "") {
		t.Fatal("secret headers must redact")
	}
	if redactHeaderName("Content-Type", "") || redactHeaderName("X-Request-Id", "") {
		t.Fatal("non-secret headers must not redact")
	}
}
