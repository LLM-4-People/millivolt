package main

import (
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/storage"
)

func TestPauseSafetyAndBodies(t *testing.T) {
	pauseDocument := `{"paused":false,"queued":0,"holds":[],"default_max_queued":10,"until":null}`
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/pause": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, pauseDocument)
				return
			}
			body, _ := io.ReadAll(r.Body)
			_, _ = io.WriteString(w, `{"ok":true,"paused":true,"holds":[]}`)
			_ = body
		},
	})
	if code, _, _ := runCLI(t, "--url", fixture.url(), "pause", "on", "--yes"); code != 2 {
		t.Fatalf("pause on without scope = %d, want 2", code)
	}
	if code, _, _ := runCLI(t, "--url", fixture.url(), "pause", "on", "--all"); code != 0 {
		t.Fatalf("pause on preview = %d", code)
	}
	if got := fixture.callCount("/admin/pause"); got != 0 {
		t.Fatalf("pause preview made %d calls", got)
	}
	code, _, _ := runCLI(t, "--url", fixture.url(), "pause", "on", "--all", "--yes")
	if code != 0 {
		t.Fatalf("pause on = %d", code)
	}
	calls := fixture.callsFor("/admin/pause")
	assertJSONEqual(t, calls[len(calls)-1].Body, `{"paused":true,"all":true}`)

	code, _, _ = runCLI(t, "--url", fixture.url(), "pause", "on", "--client", "a,b", "--provider", "p",
		"--duration", "15m", "--max-queued", "10", "--yes")
	if code != 0 {
		t.Fatalf("scoped pause = %d", code)
	}
	calls = fixture.callsFor("/admin/pause")
	assertJSONEqual(t, calls[len(calls)-1].Body,
		`{"paused":true,"clients":["a","b"],"providers":["p"],"duration":"15m","max_queued":10}`)

	if code, _, _ := runCLI(t, "--url", fixture.url(), "pause", "off", "--provider", "p", "--yes"); code != 2 {
		t.Fatalf("pause off with scope = %d, want 2", code)
	}
	if code, _, _ := runCLI(t, "--url", fixture.url(), "pause", "off", "--id", "h", "--all", "--yes"); code != 2 {
		t.Fatalf("pause off id+all = %d, want 2", code)
	}
	code, _, _ = runCLI(t, "--url", fixture.url(), "pause", "off", "--all", "--yes")
	if code != 0 {
		t.Fatalf("pause off = %d", code)
	}
	calls = fixture.callsFor("/admin/pause")
	assertJSONEqual(t, calls[len(calls)-1].Body, `{"paused":false}`)
}

func TestThrottleQuotaDrainDebug(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/throttle":    jsonHandler(t, `{"ok":true,"throttles":[],"active":false}`),
		"/admin/quota":       jsonHandler(t, `{"ok":true,"storms":[]}`),
		"/admin/queue/drain": jsonHandler(t, `{"drained_through":4,"provider":"p"}`),
		"/admin/debug":       jsonHandler(t, `{"ok":true,"enabled":false,"sessions":[]}`),
	})
	code, _, _ := runCLI(t, "--url", fixture.url(), "throttle", "set", "--provider", "p",
		"--concurrency", "2", "--requests", "100", "--request-window", "1m",
		"--tokens", "1000", "--token-window", "2m", "--anti-burst=false",
		"--ramp-duration", "30s", "--yes")
	if code != 0 {
		t.Fatalf("throttle set = %d", code)
	}
	calls := fixture.callsFor("/admin/throttle")
	assertJSONEqual(t, calls[len(calls)-1].Body, `{"provider":"p","concurrency":2,"requests":100,
		"request_window":"1m","tokens":1000,"token_window":"2m","anti_burst":false,"ramp_duration":"30s"}`)
	if code, _, _ := runCLI(t, "--url", fixture.url(), "throttle", "set", "--provider", "p"); code != 2 {
		t.Fatalf("empty throttle set = %d, want 2", code)
	}

	code, _, _ = runCLI(t, "--url", fixture.url(), "quota", "resume", "--provider", "p", "--yes")
	if code != 0 {
		t.Fatalf("quota resume = %d", code)
	}
	calls = fixture.callsFor("/admin/quota")
	assertJSONEqual(t, calls[len(calls)-1].Body, `{"provider":"p","resume":true}`)

	code, _, _ = runCLI(t, "--url", fixture.url(), "drain", "--provider", "p", "--yes")
	if code != 0 {
		t.Fatalf("drain = %d", code)
	}
	calls = fixture.callsFor("/admin/queue/drain")
	assertJSONEqual(t, calls[len(calls)-1].Body, `{"provider":"p"}`)
	if code, _, _ := runCLI(t, "--url", fixture.url(), "drain", "--all", "--client", "c", "--yes"); code != 2 {
		t.Fatalf("ambiguous drain = %d, want 2", code)
	}

	if code, _, _ := runCLI(t, "--url", fixture.url(), "debug", "on", "--yes"); code != 2 {
		t.Fatalf("unscoped debug on = %d, want 2", code)
	}
	if code, _, _ := runCLI(t, "--url", fixture.url(), "debug", "on", "--all", "--yes"); code != 2 {
		t.Fatalf("global debug on = %d, want 2", code)
	}
	code, preview, _ := runCLI(t, "--url", fixture.url(), "debug", "on", "--client", "c")
	if code != 0 || !strings.Contains(preview, "sensitive") {
		t.Fatalf("debug preview = %d, %q", code, preview)
	}
	code, _, _ = runCLI(t, "--url", fixture.url(), "debug", "on", "--client", "c", "--yes")
	if code != 0 {
		t.Fatalf("debug on = %d", code)
	}
	calls = fixture.callsFor("/admin/debug")
	assertJSONEqual(t, calls[len(calls)-1].Body, `{"enabled":true,"clients":["c"]}`)
	if code, _, _ := runCLI(t, "--url", fixture.url(), "debug", "off", "--provider", "p", "--yes"); code != 2 {
		t.Fatalf("debug off with scope = %d, want 2", code)
	}
	if code, _, _ := runCLI(t, "--url", fixture.url(), "debug", "off", "--id", "d", "--all", "--yes"); code != 2 {
		t.Fatalf("debug off id+all = %d, want 2", code)
	}
	code, _, _ = runCLI(t, "--url", fixture.url(), "debug", "off", "--all", "--yes")
	if code != 0 {
		t.Fatalf("debug off all = %d", code)
	}
	calls = fixture.callsFor("/admin/debug")
	assertJSONEqual(t, calls[len(calls)-1].Body, `{"enabled":false}`)
}

func TestPurgeNarrowingAndPreview(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/purge/count": jsonHandler(t, `{"count":7}`),
		"/admin/purge":       jsonHandler(t, `{"ok":true}`),
	})
	code, _, _ := runCLI(t, "--url", fixture.url(), "purge", "--client", "acme", "--before-ms", "123", "--has-error", "--yes")
	if code != 0 {
		t.Fatalf("purge = %d", code)
	}
	counts := fixture.callsFor("/admin/purge/count")
	deletes := fixture.callsFor("/admin/purge")
	assertJSONEqual(t, counts[len(counts)-1].Body, `{"client":"acme","before_ms":123,"has_error":true}`)
	assertJSONEqual(t, deletes[len(deletes)-1].Body, `{"client":"acme","before_ms":123,"has_error":true}`)

	code, _, _ = runCLI(t, "--url", fixture.url(), "purge", "--all", "--yes")
	if code != 0 {
		t.Fatalf("purge all = %d", code)
	}
	counts = fixture.callsFor("/admin/purge/count")
	deletes = fixture.callsFor("/admin/purge")
	if strings.TrimSpace(counts[len(counts)-1].Body) != "" {
		t.Fatalf("purge all count body = %q, want no body", counts[len(counts)-1].Body)
	}
	if strings.TrimSpace(deletes[len(deletes)-1].Body) != "" {
		t.Fatalf("purge all delete body = %q, want no body", deletes[len(deletes)-1].Body)
	}

	before := fixture.callCount("/admin/purge")
	code, preview, _ := runCLI(t, "--url", fixture.url(), "purge", "--client", "acme")
	if code != 0 || !strings.Contains(preview, "Preview only") {
		t.Fatalf("purge preview = %d, %q", code, preview)
	}
	if got := fixture.callCount("/admin/purge"); got != before {
		t.Fatalf("purge preview deleted rows (%d -> %d)", before, got)
	}
	for _, args := range [][]string{
		{"purge", "--status", "0", "--yes"},
		{"purge", "--before-ms", "0", "--yes"},
		{"purge", "--all", "--client", "acme", "--yes"},
		{"purge", "--yes"},
	} {
		code, _, _ := runCLI(t, append([]string{"--url", fixture.url()}, args...)...)
		if code != 2 {
			t.Errorf("%v = %d, want 2", args, code)
		}
	}
}

func TestPurgeFilterKeysMatchStorageOwner(t *testing.T) {
	typ := reflect.TypeOf(storage.PurgeFilter{})
	allowed := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			allowed[tag] = true
		}
	}
	values := map[string]any{
		"client": "c", "provider": "p", "model": "m",
		"conversation-id": "conv", "error-type": "rate_limit",
		"status": int64(500), "before-ms": int64(1000), "after-ms": int64(500),
		"has-error": true, "debug": true,
	}
	set := map[string]bool{}
	for key := range values {
		set[key] = true
	}
	filter, err := purgeFilter(&Context{Flags: flagValues{values: values, set: set}})
	if err != nil {
		t.Fatal(err)
	}
	if filter.IsEmpty() {
		t.Fatal("a fully supplied filter must not be empty")
	}
	document := filter.Document()
	if len(document) != len(allowed) {
		t.Fatalf("filter covers %d keys, storage owns %d: %v", len(document), len(allowed), document)
	}
	for key := range document {
		if !allowed[key] {
			t.Errorf("filter key %q is not a storage.PurgeFilter field", key)
		}
	}
}

func TestThrottleRejectsUnknownProvider(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/debug":    jsonHandler(t, `{"ok":true,"enabled":false,"sessions":[],"known_providers":["known"]}`),
		"/admin/throttle": jsonHandler(t, `{"ok":true,"throttles":[]}`),
	})
	code, _, stderr := runCLI(t, "--url", fixture.url(), "throttle", "set", "--provider", "typo", "--concurrency", "1", "--yes")
	if code != 1 || !strings.Contains(stderr, "not one of the known providers") {
		t.Fatalf("unknown provider = %d, stderr %q", code, stderr)
	}
	if fixture.callCount("/admin/throttle") != 0 {
		t.Fatal("unknown provider reached the throttle write")
	}
}

func TestPurgeRefusesCountDriftAndInvalidCount(t *testing.T) {
	var counts int
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/purge/count": func(w http.ResponseWriter, _ *http.Request) {
			counts++
			if counts == 1 {
				_, _ = io.WriteString(w, `{"count":7}`)
				return
			}
			_, _ = io.WriteString(w, `{"count":8}`)
		},
		"/admin/purge": jsonHandler(t, `{"ok":true}`),
	})
	code, _, stderr := runCLI(t, "--url", fixture.url(), "purge", "--client", "acme", "--yes")
	if code != 1 || !strings.Contains(stderr, "row count changed from 7 to 8") {
		t.Fatalf("count drift = %d, stderr %q", code, stderr)
	}
	if got := fixture.callCount("/admin/purge"); got != 0 {
		t.Fatalf("count drift deleted rows: %d calls", got)
	}

	for _, payload := range []string{`{}`, `{"count":-1}`, `{"count":1.5}`, `{"count":"7"}`} {
		counts = -100 // make every count response the same invalid document
		fixture := newProxy(t, map[string]http.HandlerFunc{
			"/admin/purge/count": jsonHandler(t, payload),
			"/admin/purge":       jsonHandler(t, `{"ok":true}`),
		})
		code, _, stderr := runCLI(t, "--url", fixture.url(), "purge", "--client", "acme", "--yes")
		if code != 1 || stderr == "" {
			t.Errorf("count %s = %d, stderr %q, want a failure", payload, code, stderr)
		}
		if got := fixture.callCount("/admin/purge"); got != 0 {
			t.Errorf("invalid count %s deleted rows: %d calls", payload, got)
		}
	}
}

func TestPauseEditAndThrottleWindowOnlyBodies(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/pause": jsonHandler(t, `{"ok":true,"paused":true,"holds":[]}`),
		"/admin/debug": jsonHandler(t, `{"ok":true,"enabled":false,"sessions":[],"known_providers":["p"]}`),
		"/admin/throttle": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"ok":true,"throttles":[{"provider":"p","requests":5}],"known_providers":["p"]}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"throttles":[]}`)
		},
		"/admin/queue/drain": jsonHandler(t, `{"ok":true}`),
	})
	code, _, _ := runCLI(t, "--url", fixture.url(), "pause", "on", "--id", "h1", "--duration", "1h", "--yes")
	if code != 0 {
		t.Fatalf("pause edit = %d", code)
	}
	calls := fixture.callsFor("/admin/pause")
	assertJSONEqual(t, calls[len(calls)-1].Body, `{"paused":true,"id":"h1","duration":"1h"}`)

	code, _, _ = runCLI(t, "--url", fixture.url(), "pause", "off", "--id", "h1", "--yes")
	if code != 0 {
		t.Fatalf("pause off id = %d", code)
	}
	calls = fixture.callsFor("/admin/pause")
	assertJSONEqual(t, calls[len(calls)-1].Body, `{"paused":false,"id":"h1"}`)

	code, _, _ = runCLI(t, "--url", fixture.url(), "throttle", "clear", "--provider", "p", "--yes")
	if code != 0 {
		t.Fatalf("throttle clear = %d", code)
	}
	calls = fixture.callsFor("/admin/throttle")
	assertJSONEqual(t, calls[len(calls)-1].Body, `{"provider":"p","clear":true}`)

	code, _, _ = runCLI(t, "--url", fixture.url(), "throttle", "set", "--provider", "p", "--request-window", "1m", "--yes")
	if code != 0 {
		t.Fatalf("window-only throttle = %d", code)
	}
	calls = fixture.callsFor("/admin/throttle")
	assertJSONEqual(t, calls[len(calls)-1].Body, `{"provider":"p","request_window":"1m"}`)

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"drain", "--all", "--yes"}, `{"all":true}`},
		{[]string{"drain", "--model", "m", "--yes"}, `{"model":"m"}`},
		{[]string{"drain", "--client", "c", "--yes"}, `{"client":"c"}`},
	} {
		code, _, _ := runCLI(t, append([]string{"--url", fixture.url()}, tc.args...)...)
		if code != 0 {
			t.Errorf("%v = %d", tc.args, code)
			continue
		}
		calls := fixture.callsFor("/admin/queue/drain")
		assertJSONEqual(t, calls[len(calls)-1].Body, tc.want)
	}
}
