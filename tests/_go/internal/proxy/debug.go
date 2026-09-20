package proxy

import (
	"bytes"
	"compress/gzip"
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

// Without a durable store there is no capture to fetch: the drawer keeps its
// 404, and its body names the one storage-disabled cause with the sentinel's
// canonical message (the old hand-written "no durable store" wording drifted
// from every other surface).
func TestHandleDebugCaptureWithoutStoreReportsSentinelText(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandleDebugCapture(rr, httptest.NewRequest(http.MethodGet, "/admin/debug/capture?id=x", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with no store wired", rr.Code)
	}
	var decoded struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("capture 404 body is not valid JSON: %v (%q)", err, rr.Body.String())
	}
	if decoded.Error != storage.ErrStorageDisabled.Error() {
		t.Fatalf("capture 404 body = %q, want the storage-disabled sentinel message", decoded.Error)
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
	cfg := config.Default()
	if st["ttl"] != config.FormatDuration(cfg.DebugCaptureTTL) {
		t.Fatalf("GET ttl = %v, want %s", st["ttl"], config.FormatDuration(cfg.DebugCaptureTTL))
	}
	if st["max_bytes"] != config.FormatByteSize(int64(cfg.DebugCaptureMaxBytes)) {
		t.Fatalf("GET max_bytes = %v, want %s", st["max_bytes"], config.FormatByteSize(int64(cfg.DebugCaptureMaxBytes)))
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

// debugSessionOf posts a HandleDebug body and returns the single session the
// state document reports, failing on any non-200 or a missing/ambiguous
// session list.
func debugSessionOf(t *testing.T, p *Server, body string) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	p.HandleDebug(rr, httptest.NewRequest(http.MethodPost, "/admin/debug", strings.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("HandleDebug %s = %d %s", body, rr.Code, rr.Body.String())
	}
	var st struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("debug state is not JSON: %v (%s)", err, rr.Body.String())
	}
	if len(st.Sessions) != 1 {
		t.Fatalf("sessions = %v, want exactly 1", st.Sessions)
	}
	return st.Sessions[0]
}

// TestHandleDebugReplaceMergesUntil pins mergeUntil's edit merge on the debug
// surface, mirroring the pause replace rows: restating the SAME duration while
// the previous window is still live keeps the previous deadline (an
// extend-in-place edit must not reset the clock), while a NEW duration
// re-anchors a fresh window from now. The deadline comparisons run on the
// stored persistedDebug.Until (nanosecond time.Time): the served document
// renders RFC 3339 at second precision, which would hide a sub-second
// re-anchor from a string comparison.
func TestHandleDebugReplaceMergesUntil(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})

	// Same duration restated: until must not move.
	seed := debugSessionOf(t, p, `{"enabled":true,"clients":["client-a"],"duration":"1h"}`)
	id, _ := seed["id"].(string)
	if id == "" {
		t.Fatalf("seed session has no id: %v", seed)
	}
	if seed["until"] == nil {
		t.Fatalf("seed session has no until despite duration 1h: %v", seed)
	}
	seedUntil := p.debug.sessions[0].Until
	if seedUntil.IsZero() {
		t.Fatal("seed session stored a zero Until despite duration 1h")
	}
	debugSessionOf(t, p, `{"enabled":true,"id":"`+id+`","clients":["client-a"],"duration":"1h"}`)
	if kept := p.debug.sessions[0].Until; !kept.Equal(seedUntil) {
		t.Fatalf("same-duration edit reset until %v -> %v, want the previous window kept", seedUntil, kept)
	}

	// New duration: until must re-anchor from now (15m lands before the old
	// 1h window, so a stale kept deadline cannot pass as a fresh one).
	fresh := debugSessionOf(t, p, `{"enabled":true,"id":"`+id+`","clients":["client-a"],"duration":"15m"}`)
	if fresh["duration"] != "15m" {
		t.Fatalf("edited duration = %v, want 15m", fresh["duration"])
	}
	freshUntil := p.debug.sessions[0].Until
	if freshUntil.Equal(seedUntil) || !freshUntil.Before(seedUntil) {
		t.Fatalf("new-duration edit kept until %v, want a re-anchored window before the old %v", freshUntil, seedUntil)
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
	p.pause.models.add(canonicalModel("claude-4.5-sonnet-thinking"))
	p.pause.models.add(canonicalModel("claude-sonnet-4-5"))
	p.pause.models.add(canonicalModel(" grok-4.6-fast "))
	p.pause.models.add(canonicalModel("grok-4.6"))
	got := p.pause.models.list()
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

// openProxyTestStore opens a Store at path with the Default-derived Options
// the stored-proxy tests share (channel/batch caps, flush cadence, query
// timeout): the debug and pause persist/restore twins, the throttle
// persist/restore test, and the debug capture test. It returns the opened
// store and the options it opened with; the contract row below pins the
// derivation. Close stays at each site: the persist/restore choreography
// closes mid-test with an error check before reopening.
func openProxyTestStore(t *testing.T, path string) (*storage.Store, storage.Options) {
	t.Helper()
	d := config.Default()
	opts := storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: d.StorageFlushInterval,
		QueryTimeout:  d.StorageQueryTimeout,
	}
	store, err := storage.Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	return store, opts
}

// TestProxyStoreOptsDeriveFromDefault pins openProxyTestStore's Options
// derivation: the four Default-derived fields (channel/batch caps, flush
// cadence, query timeout) must track config.Default(), so hardcoding one
// in the helper reddens here.
func TestProxyStoreOptsDeriveFromDefault(t *testing.T) {
	store, opts := openProxyTestStore(t, filepath.Join(t.TempDir(), "opts.db"))
	t.Cleanup(func() { store.Close() })
	d := config.Default()
	if opts.WriteChanCap != d.StorageWriteChanCap {
		t.Errorf("WriteChanCap = %d, want config.Default().StorageWriteChanCap (%d)", opts.WriteChanCap, d.StorageWriteChanCap)
	}
	if opts.BatchCap != d.StorageBatchCap {
		t.Errorf("BatchCap = %d, want config.Default().StorageBatchCap (%d)", opts.BatchCap, d.StorageBatchCap)
	}
	if opts.FlushInterval != d.StorageFlushInterval {
		t.Errorf("FlushInterval = %v, want config.Default().StorageFlushInterval (%v)", opts.FlushInterval, d.StorageFlushInterval)
	}
	if opts.QueryTimeout != d.StorageQueryTimeout {
		t.Errorf("QueryTimeout = %v, want config.Default().StorageQueryTimeout (%v)", opts.QueryTimeout, d.StorageQueryTimeout)
	}
}

// persistRestoreProxy drives the admin persist/restore choreography the
// debug and pause twins share: open a Default-derived store at <dir>/<name>,
// attach it to a fresh proxy, POST via post (each site owns its endpoint
// and payload and returns its recorder), require 200, close with an error
// check, reopen the database, and return the proxy that restored from the
// reopened store. Each site keeps its own restore assertions.
func persistRestoreProxy(t *testing.T, name string, post func(p *Server) *httptest.ResponseRecorder) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	d := config.Default()
	store, _ := openProxyTestStore(t, path)
	p := New(d, metrics.Noop{})
	p.AttachPausePersist(store)
	rr := post(p)
	if rr.Code != 200 {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.Bytes())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2, _ := openProxyTestStore(t, path)
	t.Cleanup(func() { store2.Close() })
	p2 := New(d, metrics.Noop{})
	p2.AttachPausePersist(store2)
	return p2
}

func TestDebugPersistsAndRestores(t *testing.T) {
	p2 := persistRestoreProxy(t, "debug.db", func(p *Server) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		p.HandleDebug(rr, httptest.NewRequest(http.MethodPost, "/admin/debug",
			strings.NewReader(`{"enabled":true,"clients":["client-a"],"duration":"1h"}`)))
		return rr
	})
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
	store, _ := openProxyTestStore(t, path)
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

	// Poll the finalized record: live publishes fire at acceptance with
	// StatusCode still 0, so the old poll of the live view for a 200 could
	// never match and only burned the deadline without verifying anything.
	// finishDebugTap saves the capture before Record runs, so observing the
	// finalized record also proves the capture is durable.
	deadline := time.Now().Add(2 * time.Second)
	var rec *metrics.Record
	for time.Now().Before(deadline) {
		if rec = spy.final.Load(); rec != nil && rec.Debug && rec.StatusCode == 200 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rec == nil || !rec.Debug || rec.StatusCode != 200 {
		t.Fatal("finalized record never observed with debug=true and status 200")
	}
	if live := spy.last.Load(); live == nil || !live.Debug {
		t.Fatal("live record never published with debug=true")
	}

	got := httptest.NewRecorder()
	p.HandleDebugCapture(got, httptest.NewRequest(http.MethodGet, "/admin/debug/capture?id="+rec.ID, nil))
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
	// The rendered timestamps must carry the explicit UTC designator: the
	// drawer renders them verbatim, and dropping the .UTC() call changes the
	// zone silently (the same mutation class as rfc3339OrNil's pin).
	var doc struct {
		CapturedAt string `json:"captured_at"`
		ExpiresAt  string `json:"expires_at"`
		Timing     struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"timing"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &doc); err != nil {
		t.Fatalf("capture doc: %v", err)
	}
	for name, s := range map[string]string{"captured_at": doc.CapturedAt, "expires_at": doc.ExpiresAt, "timing.start": doc.Timing.Start, "timing.end": doc.Timing.End} {
		if !strings.HasSuffix(s, "Z") {
			t.Fatalf("%s = %q, want the UTC Z designator", name, s)
		}
		if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
			t.Fatalf("%s = %q does not parse as RFC 3339: %v", name, s, err)
		}
	}
}

func TestRedactHeaderName(t *testing.T) {
	if !redactHeaderName("Authorization", "") || !redactHeaderName("X-Api-Key", "") || !redactHeaderName("Cookie", "") {
		t.Fatal("secret headers must redact")
	}
	if redactHeaderName("Content-Type", "") || redactHeaderName("X-Request-Id", "") {
		t.Fatal("non-secret headers must not redact")
	}
	// Rate-limit accounting headers carry "tokens" in the name but no
	// credential; both provider families must pass, plus the client
	// queue-limit control header.
	for _, name := range []string{
		"x-ratelimit-remaining-tokens", "x-ratelimit-reset-tokens",
		"anthropic-ratelimit-tokens-limit", "anthropic-ratelimit-tokens-remaining",
		"anthropic-ratelimit-tokens-reset", "x-proxy-limit-tokens",
	} {
		if redactHeaderName(name, "") {
			t.Errorf("%s must not redact", name)
		}
	}
	// Accepted residual, pinned: the ratelimit exemptions are prefix-wide,
	// so a token-NAMED header under them stays retained (x-ratelimit-token,
	// anthropic-ratelimit-token). The upstream already holds the credential
	// and any header name can carry data; see redactHeaderName.
	for _, name := range []string{
		"x-ratelimit-token", "anthropic-ratelimit-token",
	} {
		if redactHeaderName(name, "") {
			t.Errorf("%s must stay retained (prefix-wide exemption residual)", name)
		}
	}
	// Deny by default stays: credential-bearing proxy controls and any
	// unknown token-bearing header remain redacted.
	for _, name := range []string{
		"x-proxy-refresh-token", "x-proxy-access-token", "x-proxy-key",
		"x-proxy-headers", "x-session-token",
	} {
		if !redactHeaderName(name, "") {
			t.Errorf("%s must redact", name)
		}
	}
}

// The debug body cap policy: capBytes keeps the ORIGINAL observed byte count
// and an explicit truncation flag when the limit cuts the body; the
// cappedWriter reports the total it saw (seen), not the retained prefix;
// and a limit that cuts mid-rune surfaces the U+FFFD replacement - the
// retained view is always valid UTF-8 for JSON display, while Bytes and
// Truncated stay the caller's own facts, never recomputed by the sanitize.
func TestSanitizeDebugBodyPolicyRows(t *testing.T) {
	over := capBytes([]byte("abcdef"), 3)
	if over.Raw != "abc" || over.Bytes != 6 || !over.Truncated {
		t.Fatalf("capBytes over-limit = %+v, want {Raw:abc Bytes:6 Truncated:true}", over)
	}
	atLimit := capBytes([]byte("abc"), 3)
	if atLimit.Raw != "abc" || atLimit.Bytes != 3 || atLimit.Truncated {
		t.Fatalf("capBytes at-limit = %+v, want {Raw:abc Bytes:3 Truncated:false}", atLimit)
	}
	cw := &cappedWriter{limit: 4}
	for _, chunk := range []string{"1234", "5678"} {
		if _, err := cw.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	seen := cw.body()
	if seen.Raw != "1234" || seen.Bytes != 8 || !seen.Truncated {
		t.Fatalf("cappedWriter over-cap = %+v, want {Raw:1234 Bytes:8 Truncated:true}", seen)
	}
	// The limit lands mid-rune: the invalid half-rune becomes the U+FFFD
	// replacement while the original size and truncation flag ride unchanged.
	cut := capBytes([]byte("a\xc3\xa9b"), 2)
	if cut.Raw != "a\ufffd" || cut.Bytes != 4 || !cut.Truncated {
		t.Fatalf("mid-rune cut = %+v, want {Raw:a\\uFFFD Bytes:4 Truncated:true}", cut)
	}
}

// TestHandleDebugCaptureDownloadServesGzipArtifact pins the download=1 mode:
// the same handler that serves the drawer's render bytes switches to a saved
// gzip artifact, while the render mode stays byte-identical, the 404 rows
// hold in both modes, and a malformed flag is rejected instead of guessed.
func TestHandleDebugCaptureDownloadServesGzipArtifact(t *testing.T) {
	store, _ := openProxyTestStore(t, filepath.Join(t.TempDir(), "capture-download.db"))
	p := New(config.Default(), metrics.Noop{})
	p.AttachPausePersist(store)

	const payload = `{"schema":"millivolt.debug/v1","id":"cap-dl","captured_at":"2026-09-20T12:34:56.789012345Z","expires_at":"2026-09-21T12:34:56Z"}`
	if err := store.SaveDebugCapture(t.Context(), "cap-dl", "sess",
		time.Now().UnixMilli(), time.Now().Add(time.Hour).UnixMilli(), []byte(payload)); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	p.HandleDebugCapture(w, httptest.NewRequest(http.MethodGet, "/admin/debug/capture?id=cap-dl", nil))
	if w.Code != 200 || w.Body.String() != payload {
		t.Fatalf("render mode changed: status=%d body=%q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("render Content-Type = %q", ct)
	}
	if dispo := w.Header().Get("Content-Disposition"); dispo != "" {
		t.Fatalf("render mode sets Content-Disposition %q", dispo)
	}

	w = httptest.NewRecorder()
	p.HandleDebugCapture(w, httptest.NewRequest(http.MethodGet, "/admin/debug/capture?id=cap-dl&download=1", nil))
	if w.Code != 200 {
		t.Fatalf("download status = %d body=%q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Fatalf("download Content-Type = %q, want application/gzip", ct)
	}
	if dispo, want := w.Header().Get("Content-Disposition"), `attachment; filename="millivolt-debug-20260920-123456.json.gz"`; dispo != want {
		t.Fatalf("download Content-Disposition = %q, want %q", dispo, want)
	}
	zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("download body is not gzip: %v (%q)", err, w.Body.String())
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip capture artifact: %v", err)
	}
	if string(raw) != payload {
		t.Fatalf("gunzipped artifact = %q, want the capture JSON", raw)
	}

	for _, tc := range []struct{ name, query string }{
		{"render absent capture", "?id=missing"},
		{"download absent capture", "?id=missing&download=1"},
	} {
		w := httptest.NewRecorder()
		p.HandleDebugCapture(w, httptest.NewRequest(http.MethodGet, "/admin/debug/capture"+tc.query, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", tc.name, w.Code)
		}
	}
	bare := New(config.Default(), metrics.Noop{})
	for _, tc := range []struct{ name, query string }{
		{"render no durable store", "?id=cap-dl"},
		{"download no durable store", "?id=cap-dl&download=1"},
	} {
		w := httptest.NewRecorder()
		bare.HandleDebugCapture(w, httptest.NewRequest(http.MethodGet, "/admin/debug/capture"+tc.query, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", tc.name, w.Code)
		}
	}

	w = httptest.NewRecorder()
	p.HandleDebugCapture(w, httptest.NewRequest(http.MethodGet, "/admin/debug/capture?id=cap-dl&download=yes", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed download flag: status = %d, want 400", w.Code)
	}
}

// TestHandleDebugCaptureDownloadNamesArtifact pins the artifact naming: a
// readable captured_at (UTC RFC 3339Nano in the stored document, any offset
// converted to UTC) names the file by its second-granularity timestamp, and
// every decode or parse failure falls back to the record id.
func TestHandleDebugCaptureDownloadNamesArtifact(t *testing.T) {
	store, _ := openProxyTestStore(t, filepath.Join(t.TempDir(), "capture-name.db"))
	p := New(config.Default(), metrics.Noop{})
	p.AttachPausePersist(store)
	expires := time.Now().Add(time.Hour).UnixMilli()
	for _, tc := range []struct{ id, payload, want string }{
		{"name-utc", `{"id":"name-utc","captured_at":"2026-09-20T12:34:56.789012345Z"}`, "millivolt-debug-20260920-123456.json.gz"},
		{"name-offset", `{"id":"name-offset","captured_at":"2026-09-20T14:34:56+02:00"}`, "millivolt-debug-20260920-123456.json.gz"},
		{"fb-no-field", `{"id":"fb-no-field"}`, "millivolt-debug-fb-no-field.json.gz"},
		{"fb-garbage-time", `{"id":"fb-garbage-time","captured_at":"not a timestamp"}`, "millivolt-debug-fb-garbage-time.json.gz"},
		{"fb-not-json", `not json at all`, "millivolt-debug-fb-not-json.json.gz"},
	} {
		if err := store.SaveDebugCapture(t.Context(), tc.id, "sess", time.Now().UnixMilli(), expires, []byte(tc.payload)); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		p.HandleDebugCapture(w, httptest.NewRequest(http.MethodGet, "/admin/debug/capture?id="+tc.id+"&download=1", nil))
		want := `attachment; filename="` + tc.want + `"`
		if w.Code != 200 || w.Header().Get("Content-Disposition") != want {
			t.Errorf("%s: status=%d disposition=%q, want %q", tc.id, w.Code, w.Header().Get("Content-Disposition"), want)
			continue
		}
		zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
		if err != nil {
			t.Errorf("%s: artifact is not gzip: %v", tc.id, err)
			continue
		}
		raw, err := io.ReadAll(zr)
		if err != nil || string(raw) != tc.payload {
			t.Errorf("%s: gunzipped artifact = %q err=%v, want the stored payload", tc.id, raw, err)
		}
	}
}
