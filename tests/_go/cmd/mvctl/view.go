package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/mcp"
)

const bootstrapJSON = `{
  "kpi": {"requests": 12, "errors": 1, "in_flight": 2, "cost": 0.5,
    "cost_per_req": 0.04, "cost_per_mtok": null,
    "input_tokens": 100, "output_tokens": 50, "cache_read_tokens": 10,
    "reasoning_tokens": 5, "avg_ttft_ms": 120, "avg_tps": 30},
  "storage": {"enabled": true, "dropped": 0, "totals_degraded": false},
  "pause": {"paused": false, "queued": 0},
  "throttle": {"active": false, "throttles": []},
  "debug": {"enabled": false, "sessions": []},
  "dashboard_version": "abcdef0123456789",
  "storm": {"quota_gates": []},
  "records": [{"id":"r1","client":"c","prompt_preview":"SECRET PREVIEW",
    "response_headers":{"Set-Cookie":["session=secret"]}}],
  "in_flight_records": [{"id":"r2","response_headers":{"Authorization":["Bearer secret"]}}],
  "model_canon": {"rules": []},
  "dash": {"providers": {"p": {"headers": {"Authorization": "Bearer dash-secret"}}}}
}`

func TestStatusAndOverview(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/healthz":           jsonHandler(t, `{"ok":true}`),
		"/metrics/bootstrap": jsonHandler(t, bootstrapJSON),
	})
	code, stdout, stderr := runCLI(t, "--url", fixture.url(), "status", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("status = %d, stderr %q", code, stderr)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("decode status JSON: %v\n%s", err, stdout)
	}
	for _, key := range []string{"kpi", "storage", "storm", "dashboard_version"} {
		if _, ok := document[key]; !ok {
			t.Errorf("status --json dropped server field %q", key)
		}
	}
	for _, key := range []string{"records", "in_flight_records", "dash", "model_canon"} {
		if _, ok := document[key]; ok {
			t.Errorf("status --json exposed %q, want the safe project", key)
		}
	}
	code, human, _ := runCLI(t, "--url", fixture.url(), "status")
	if code != 0 || !strings.Contains(human, "millivolt status") || !strings.Contains(human, "abcdef012345...") {
		t.Fatalf("status = %d, %q", code, human)
	}
	if strings.Contains(human, "SECRET PREVIEW") || strings.Contains(human, "dash-secret") {
		t.Fatalf("human status leaked a captured value or config secret: %q", human)
	}
	if strings.Contains(stdout, "SECRET PREVIEW") || strings.Contains(stdout, "dash-secret") {
		t.Fatalf("status --json leaked a captured value or config secret: %q", stdout)
	}
	code, human, _ = runCLI(t, "--url", fixture.url(), "overview")
	if code != 0 || !strings.Contains(human, "Input tokens") || strings.Contains(human, "\x1b") {
		t.Fatalf("overview = %d, %q", code, human)
	}
	code, stdout, _ = runCLI(t, "--url", fixture.url(), "overview", "--json")
	if code != 0 || strings.Contains(stdout, "SECRET PREVIEW") || strings.Contains(stdout, "dash-secret") ||
		strings.Contains(stdout, "Bearer secret") || strings.Contains(stdout, `"records"`) ||
		strings.Contains(stdout, `"in_flight_records"`) || strings.Contains(stdout, `"dash"`) {
		t.Fatalf("overview --json leaked the raw bootstrap: %q", stdout)
	}
}

func TestStatusReportsUnhealthy(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/healthz": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		"/metrics/bootstrap": jsonHandler(t, bootstrapJSON),
	})
	code, stdout, stderr := runCLI(t, "--url", fixture.url(), "status")
	if code != 1 || !strings.Contains(stderr, "healthz") || !strings.Contains(stdout, "unhealthy") {
		t.Fatalf("status = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestRequestsKeepsTelemetryAndSanitizesCells(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/metrics/agg/log": func(w http.ResponseWriter, r *http.Request) {
			query := r.URL.Query()
			if got := query.Get("limit"); got != "50" {
				t.Errorf("limit = %q, want 50", got)
			}
			if query.Get("before_ms") != "" && (query.Get("before_ms") != "123" || query.Get("before_id") != "r0") {
				t.Errorf("cursor = %q/%q, want 123/r0", query.Get("before_ms"), query.Get("before_id"))
			}
			_, _ = io.WriteString(w, `{"records":[{
			  "id":"r1","start":"2026-11-05T12:00:00Z","status_code":200,
			  "provider":"evil\u001b[31m","model":"m","client":"c","stream":false,
			  "usage":{"input_tokens":12,"output_tokens":3},"duration_ms":150,
			  "ttft_ms":50,"retries":0,"prompt_preview":"SECRET PREVIEW"
			}],"more":true,"cursor_ms":123,"cursor_id":"r1"}`)
		},
	})
	code, stdout, stderr := runCLI(t, "--url", fixture.url(), "requests", "50", "--before-ms", "123", "--before-id", "r0")
	if code != 0 || stderr != "" {
		t.Fatalf("requests = %d, stderr %q", code, stderr)
	}
	if strings.Contains(stdout, "\x1b") {
		t.Fatalf("human output contains a terminal escape: %q", stdout)
	}
	if strings.Contains(stdout, "SECRET PREVIEW") {
		t.Fatal("human output leaked a prompt preview")
	}
	code, stdout, _ = runCLI(t, "--url", fixture.url(), "requests", "50", "--json")
	if code != 0 || !strings.Contains(stdout, `"input_tokens": 12`) {
		t.Fatalf("requests --json lost input_tokens: %q", stdout)
	}
	if strings.Contains(stdout, "prompt_preview") || strings.Contains(stdout, "SECRET PREVIEW") {
		t.Fatalf("requests --json leaked captured content: %q", stdout)
	}
}

func TestRequestsRejectsInvalidLimit(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/metrics/agg/log": jsonHandler(t, `{"records":[]}`),
	})
	for _, limit := range []string{"5", "501", "abc"} {
		code, _, _ := runCLI(t, "--url", fixture.url(), "requests", limit)
		if code != 2 {
			t.Errorf("requests %s = %d, want 2", limit, code)
		}
	}
	if got := fixture.callCount("/metrics/agg/log"); got != 0 {
		t.Fatalf("invalid requests reached the proxy %d times", got)
	}
}

func TestQueryDoesNotRedactTelemetry(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/metrics/query": func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("q"); got != "SELECT input_tokens FROM requests" {
				t.Errorf("q = %q", got)
			}
			_, _ = io.WriteString(w, `[{"input_tokens":42,"token_window":"1m"}]`)
		},
	})
	code, stdout, _ := runCLI(t, "--url", fixture.url(), "query", "SELECT", "input_tokens", "FROM", "requests", "--json")
	if code != 0 || !strings.Contains(stdout, `"input_tokens": 42`) || !strings.Contains(stdout, `"token_window": "1m"`) {
		t.Fatalf("query = %d, %q", code, stdout)
	}
}

func TestRedirectIsRefusedAndTokenIsNotEchoed(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		_, _ = io.WriteString(w, bootstrapJSON)
	}))
	defer target.Close()
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/healthz": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/metrics/bootstrap", http.StatusTemporaryRedirect)
		},
		"/metrics/bootstrap": jsonHandler(t, bootstrapJSON),
	})
	code, stdout, stderr := runCLI(t, "--url", fixture.url(), "status")
	if code != 1 || targetHits != 0 {
		t.Fatalf("redirect = %d, target hits %d, stdout %q, stderr %q", code, targetHits, stdout, stderr)
	}
	if !strings.Contains(stderr, "redirect") {
		t.Fatalf("redirect error did not name the refusal: %q", stderr)
	}
}

func TestServerErrorRedactsTokenEcho(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/healthz": jsonHandler(t, `{"ok":true}`),
		"/metrics/bootstrap": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"reflected `+r.Header.Get("Authorization")+` \u001b[31m"}`)
		},
	})
	code, stdout, stderr := runCLI(t, "--url", fixture.url(), "status")
	if code != 1 {
		t.Fatalf("status = %d, want 1", code)
	}
	if strings.Contains(stdout+stderr, testToken) {
		t.Fatalf("token leaked: stdout %q stderr %q", stdout, stderr)
	}
	if !strings.Contains(stderr, "[redacted]") {
		t.Fatalf("error did not report a redacted credential: %q", stderr)
	}
	if strings.Contains(stderr, "\x1b") {
		t.Fatalf("error text reached the terminal with an escape sequence: %q", stderr)
	}
}

func TestSimpleReadCommands(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/healthz":           jsonHandler(t, `{"ok":true}`),
		"/metrics/bootstrap": jsonHandler(t, bootstrapJSON),
		"/metrics/prometheus": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "millivolt_requests_total 1\n")
		},
		"/admin/pause":    jsonHandler(t, `{"paused":false,"queued":0,"holds":[],"default_max_queued":10}`),
		"/admin/throttle": jsonHandler(t, `{"ok":true,"throttles":[],"active":false}`),
		"/admin/quota":    jsonHandler(t, `{"ok":true,"storms":[]}`),
		"/admin/debug":    jsonHandler(t, `{"ok":true,"enabled":false,"sessions":[],"ttl":"15m","max_bytes":1024}`),
		"/admin/restart":  jsonHandler(t, `{"ok":true,"allowed":true}`),
	})
	for _, tc := range []struct {
		args []string
		path string
	}{
		{[]string{"health"}, "/healthz"},
		{[]string{"prometheus"}, "/metrics/prometheus"},
		{[]string{"pause"}, "/admin/pause"},
		{[]string{"throttle"}, "/admin/throttle"},
		{[]string{"quota"}, "/admin/quota"},
		{[]string{"debug"}, "/admin/debug"},
	} {
		code, stdout, stderr := runCLI(t, append([]string{"--url", fixture.url()}, tc.args...)...)
		if code != 0 || stderr != "" || strings.TrimSpace(stdout) == "" {
			t.Errorf("%v = %d, stdout %q, stderr %q", tc.args, code, stdout, stderr)
		}
		if fixture.callCount(tc.path) == 0 {
			t.Errorf("%v did not call %s", tc.args, tc.path)
		}
	}
	code, preview, _ := runCLI(t, "--url", fixture.url(), "restart")
	if code != 0 || !strings.Contains(preview, "Preview only") {
		t.Fatalf("restart preview = %d, %q", code, preview)
	}
	if fixture.callCount("/admin/restart") != 0 {
		t.Fatal("restart preview called the proxy")
	}
	code, _, _ = runCLI(t, "--url", fixture.url(), "restart", "--yes")
	if code != 0 || fixture.callCount("/admin/restart") != 1 {
		t.Fatalf("restart apply = %d, calls %d", code, fixture.callCount("/admin/restart"))
	}
}

func TestRequestsUsesTheShippedDefaultLimit(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/metrics/agg/log": func(w http.ResponseWriter, r *http.Request) {
			want := fmt.Sprintf("%d", config.Default().DashLogRows)
			if got := r.URL.Query().Get("limit"); got != want {
				t.Errorf("default limit = %q, want the shipped dash_log_rows value %s", got, want)
			}
			_, _ = io.WriteString(w, `{"records":[]}`)
		},
	})
	code, _, _ := runCLI(t, "--url", fixture.url(), "requests")
	if code != 0 || fixture.callCount("/metrics/agg/log") != 1 {
		t.Fatalf("requests = %d, calls %d", code, fixture.callCount("/metrics/agg/log"))
	}
}

func TestWatchSamplesAndInterruptExit(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/metrics/bootstrap": jsonHandler(t, bootstrapJSON),
	})
	code, stdout, _ := runCLI(t, "--url", fixture.url(), "watch", "--samples", "2", "--interval", "1ms", "--json")
	if code != 0 {
		t.Fatalf("watch = %d", code)
	}
	var document struct {
		Samples []map[string]any `json:"samples"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil || len(document.Samples) != 2 {
		t.Fatalf("watch samples = %d, %v", len(document.Samples), err)
	}
}

func TestJSONErrorShape(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/healthz": jsonHandler(t, `{"ok":true}`),
		"/metrics/bootstrap": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
		},
	})
	code, stdout, stderr := runCLI(t, "--url", fixture.url(), "status", "--json")
	if code != 1 || stderr != "" {
		t.Fatalf("json error = %d, stderr %q", code, stderr)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(stdout), &document); err != nil || document["error"] == nil {
		t.Fatalf("json error shape = %v, %v", document, err)
	}
}

func TestTimeoutBoundsARequest(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/healthz": jsonHandler(t, `{"ok":true}`),
		"/metrics/bootstrap": func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(50 * time.Millisecond)
			_, _ = io.WriteString(w, bootstrapJSON)
		},
	})
	code, _, stderr := runCLI(t, "--url", fixture.url(), "--timeout", "1ms", "status")
	if code != 1 || !strings.Contains(stderr, "deadline") {
		t.Fatalf("timeout = %d, stderr %q", code, stderr)
	}
}

func TestDebugShowPrintsCapturesNotQueued(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/debug": jsonHandler(t, `{"ok":true,"enabled":true,"sessions":[
			{"id":"d1","clients":["c"],"providers":[],"models":[],"duration":"15m",
			 "until":null,"ran_ms":1000,"captures":7}],"ttl":"15m","max_bytes":1024}`),
	})
	code, stdout, _ := runCLI(t, "--url", fixture.url(), "debug")
	if code != 0 || !strings.Contains(stdout, "captures") || !strings.Contains(stdout, "7") {
		t.Fatalf("debug show = %d, %q", code, stdout)
	}
	if strings.Contains(stdout, "queued") {
		t.Fatalf("debug show labelled session captures as queued: %q", stdout)
	}
}

func TestRequestsRejectsBadCursorAndQueryTimeout(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/metrics/agg/log": jsonHandler(t, `{"records":[]}`),
		"/metrics/query":   jsonHandler(t, `[]`),
	})
	for _, args := range [][]string{
		{"query", "SELECT 1", "--query-timeout", "-1s"},
		{"query", "SELECT 1", "--query-timeout", "0"},
		{"query", "DELETE FROM requests"},
	} {
		code, _, _ := runCLI(t, append([]string{"--url", fixture.url()}, args...)...)
		if code != 2 {
			t.Errorf("%v = %d, want 2", args, code)
		}
	}
	if fixture.callCount("/metrics/query") != 0 {
		t.Fatal("an invalid query timeout reached the proxy")
	}
	// An empty record id is a real keyset position, not an absent cursor.
	code, _, _ := runCLI(t, "--url", fixture.url(), "requests", "--before-ms", "1", "--before-id", "")
	if code != 0 {
		t.Fatalf("empty cursor = %d", code)
	}
	calls := fixture.callsFor("/metrics/agg/log")
	if got := calls[len(calls)-1].Query; !strings.Contains(got, "before_id=&") && !strings.HasSuffix(got, "before_id=") {
		t.Fatalf("empty cursor query = %q, want before_id present", got)
	}
}

func TestCanceledInFlightRequestExits130(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/healthz": jsonHandler(t, `{"ok":true}`),
	})
	t.Setenv(mcp.ProxyTokenEnv, testToken)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	code := runWithContext(ctx, []string{"--url", fixture.url(), "health"}, &stdout, &stderr)
	if code != 130 {
		t.Fatalf("canceled request = %d, want 130 (stderr %q)", code, stderr.String())
	}
}
