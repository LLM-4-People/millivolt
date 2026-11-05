package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTolerantModelDecoding(t *testing.T) {
	body := []byte(`{
		"kpi":{"requests":39275,"errors":108,"in_flight":1,"cost":28.69,
			"input_tokens":9909959702,"output_tokens":28766213,
			"cache_read_tokens":9770446257,"reasoning_tokens":15131460,
			"avg_ttft_ms":3674.27,"avg_tps":270.10},
		"storage":{"enabled":true,"dropped":0,"totals_degraded":false},
		"pause":{"paused":false,"queued":0,"holds":[]},
		"throttle":{"active":false,"throttles":[]},
		"debug":{"enabled":false},
		"dashboard_version":"4c94a75604d63700",
		"counters":{"in_flight":1,"total_requests":39275,"total_errors":108}
	}`)
	s, err := DecodeSnapshot(body)
	if err != nil {
		t.Fatal(err)
	}
	if s.KPI.Requests != 39275 || s.KPI.Cost < 28.6 || s.KPI.AvgTTFTMs == nil || *s.KPI.AvgTTFTMs < 3674 {
		t.Fatalf("kpi decoded wrong: %+v", s.KPI)
	}
	if !s.Storage.Enabled || s.DashboardVersion == "" || s.Counters.InFlight != 1 {
		t.Fatalf("snapshot sections decoded wrong: %+v", s)
	}
	if len(s.Raw) == 0 {
		t.Fatal("raw payload not retained")
	}
}

func TestNullableMetrics(t *testing.T) {
	s, err := DecodeSnapshot([]byte(`{"kpi":{"avg_ttft_ms":null,"avg_tps":null},"storage":{},"pause":{},"throttle":{},"debug":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.KPI.AvgTTFTMs != nil || s.KPI.AvgTPS != nil {
		t.Fatal("null metrics must stay nil")
	}
}

func TestRequestRowDecoder(t *testing.T) {
	body := []byte(`{"records":[
		{"id":"abc","start":"2026-11-05T10:17:13.148Z","status_code":200,
		 "provider":"opencode-go","model":"hy4","client":"js 7.19.0","stream":true,
		 "duration_ms":1234,"ttft_ms":367,"usage":{"input_tokens":120,"output_tokens":30},
		 "retries":2,"error_type":""},
		{"id":"def","status_code":500,"processing_ms":99,"usage":{},"error_type":"upstream_timeout"}
	],"more":true,"cursor_ms":123,"cursor_id":"def"}`)
	p, err := DecodeRequestPage(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rows) != 2 || !p.More || p.CursorID != "def" {
		t.Fatalf("page decoded wrong: %+v", p)
	}
	if p.Rows[0].InputTokens != 120 || p.Rows[0].OutputTokens != 30 || p.Rows[0].TTFTMs == nil || p.Rows[0].Retries != 2 {
		t.Fatalf("row 0 decoded wrong: %+v", p.Rows[0])
	}
	if p.Rows[1].DurationMs != 99 || p.Rows[1].Status != 500 || p.Rows[1].ErrorType != "upstream_timeout" {
		t.Fatalf("row 1 decoded wrong: %+v", p.Rows[1])
	}
}

func TestFormatters(t *testing.T) {
	if num(1_234_567) != "1.23M" {
		t.Fatalf("num: %s", num(1_234_567))
	}
	if cost(28.6933) != "$28.693" {
		t.Fatalf("cost: %s", cost(28.6933))
	}
	if dur(3674) != "3.7s" {
		t.Fatalf("dur: %s", dur(3674))
	}
	if bytesHuman(2*1024*1024) != "2.0MiB" {
		t.Fatalf("bytes: %s", bytesHuman(2*1024*1024))
	}
	if got := strip(bold("hello")); got != "hello" {
		t.Fatalf("strip: %q", got)
	}
	if width(bold("hello")) != 5 {
		t.Fatal("width must ignore ANSI")
	}
	if pad("ab", 5, true) != "   ab" {
		t.Fatal("pad right")
	}
	if len(strip(spark([]float64{1, 2, 3, 4, 5}, 10))) == 0 {
		t.Fatal("spark empty")
	}
}

func TestSparkResamplingHasNoArtificialTroughs(t *testing.T) {
	// A short series must be stretched across the width without zero-filled
	// buckets, which used to show up as false troughs in the chart.
	out := strip(spark([]float64{10, 10, 10, 10}, 20))
	if len([]rune(out)) != 20 {
		t.Fatalf("spark width: %d", len([]rune(out)))
	}
	for _, r := range out {
		if r != '▁' {
			t.Fatalf("flat series must render as a flat line, got %q", out)
		}
	}
	ramp := strip(spark([]float64{1, 2, 3, 4}, 8))
	if ramp == "" || strings.Contains(ramp, "▁▁▁▁▁▁▁▁") {
		t.Fatalf("ramp lost its shape: %q", ramp)
	}
}

func TestRedactTypedStructsAndTypedMaps(t *testing.T) {
	type profile struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	type configFile struct {
		Profiles map[string]profile `json:"profiles"`
		Secret   string             `json:"apikey"`
	}
	v := configFile{
		Profiles: map[string]profile{"prod": {URL: "http://x", Token: "supersecretvalue"}},
		Secret:   "anothersecret",
	}
	s := stringifyForTest(redact(v))
	for _, secret := range []string{"supersecretvalue", "anothersecret"} {
		if strings.Contains(s, secret) {
			t.Fatalf("typed struct leaked %q: %s", secret, s)
		}
	}
	if !strings.Contains(s, "[redacted]") {
		t.Fatalf("expected a redaction marker: %s", s)
	}
}

func stringifyForTest(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestTruncatePreservesUTF8(t *testing.T) {
	if got := truncate("▁▂▃▄▅▆▇█", 3); !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
	if got := truncate("provider/model", 5); !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
}

func TestTable(t *testing.T) {
	out := table([]string{"key", "value"}, [][]string{{"a", "1"}, {"longer", "222"}})
	for _, want := range []string{"key", "value", "longer", "222"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q:\n%s", want, out)
		}
	}
}

func TestRedactNeverLeaksToken(t *testing.T) {
	v := map[string]any{
		"MILLIVOLT_OPERATOR_TOKEN": "supersecretvalue",
		"nested":                   map[string]any{"authorization": "Bearer abc", "ok": 1},
		"key_hash":                 "deadbeef",
		"list":                     []any{map[string]any{"api_key": "xyz"}},
	}
	out, err := json.Marshal(redact(v))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, secret := range []string{"supersecretvalue", "Bearer abc", "deadbeef", "xyz"} {
		if strings.Contains(s, secret) {
			t.Fatalf("redaction leaked %q in %s", secret, s)
		}
	}
	if !strings.Contains(s, "[redacted]") {
		t.Fatal("expected a redaction marker")
	}
}

func TestParseValueAndFlags(t *testing.T) {
	if parseValue("true") != true || parseValue("12.5") != 12.5 || parseValue("hello") != "hello" {
		t.Fatal("parseValue")
	}
	ctx := Context{Flags: map[string]string{"provider": "openai"}}
	if flag(ctx, "provider", "x") != "openai" || flag(ctx, "missing", "fallback") != "fallback" {
		t.Fatal("flag")
	}
}

func TestPurgeFilterUsesAdminFieldNames(t *testing.T) {
	ctx := Context{Flags: map[string]string{
		"client": "mvctl-demo", "provider": "openrouter", "status": "429",
		"before_ms": "1700000000000", "has_error": "true",
	}}
	f := purgeFilter(ctx)
	if f["client"] != "mvctl-demo" || f["provider"] != "openrouter" ||
		f["status_code"] != 429 || f["before_ms"] != int64(1700000000000) || f["has_error"] != true {
		t.Fatalf("filter: %+v", f)
	}
	for _, forbidden := range []string{"clients", "since", "error", "tool"} {
		if _, ok := f[forbidden]; ok {
			t.Fatalf("forbidden key %q in purge filter", forbidden)
		}
	}
	if len(purgeFilter(Context{Flags: map[string]string{}})) != 0 {
		t.Fatal("empty flags must produce an empty filter")
	}
}

func TestDrainYesDoesNotWidenScope(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	c := NewClient(Global{BaseURL: srv.URL, Yes: true, Timeout: time.Second})
	ctx := Context{
		Ctx: context.Background(), G: Global{Yes: true}, C: c,
		Flags: map[string]string{"provider": "openai"},
	}
	if _, err := cmdDrain(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := received["all"]; ok {
		t.Fatalf("--yes widened the drain scope: %+v", received)
	}
	if received["provider"] != "openai" {
		t.Fatalf("drain scope lost: %+v", received)
	}
}

func TestDownloadRejectsTruncatedResponse(t *testing.T) {
	// A response larger than the cap must be an error, never a "successful"
	// partial archive.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, maxResponseBytes+64))
	}))
	defer srv.Close()
	c := NewClient(Global{BaseURL: srv.URL, Timeout: time.Second})
	if _, err := c.Get(context.Background(), "/big", nil); err == nil {
		t.Fatal("expected an overflow error for an oversized response")
	}
}

func TestDownloadStreamsLargeFile(t *testing.T) {
	payload := bytes.Repeat([]byte("M"), 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	c := NewClient(Global{BaseURL: srv.URL, Timeout: 5 * time.Second})
	out := filepath.Join(t.TempDir(), "backup.mvb")
	n, err := c.Download(context.Background(), "/admin/backup", nil, out)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("downloaded %d bytes, want %d", n, len(payload))
	}
	got, err := os.ReadFile(out)
	if err != nil || len(got) != len(payload) {
		t.Fatalf("file: %d bytes, err=%v", len(got), err)
	}
}

func TestDispatchAndHelp(t *testing.T) {
	cmds := CoreCommands()
	if len(cmds) < 15 {
		t.Fatal("core command registry too small")
	}
	seen := map[string]bool{}
	for _, c := range cmds {
		if seen[c.Name] {
			t.Fatalf("duplicate command %q", c.Name)
		}
		seen[c.Name] = true
		if c.Run == nil || c.Usage == "" || c.Summary == "" {
			t.Fatalf("incomplete command %q", c.Name)
		}
	}
	ctx := Context{
		Ctx:  context.Background(),
		G:    Global{Timeout: time.Second},
		C:    NewClient(Global{BaseURL: "http://127.0.0.1:1", Token: "x"}),
		Args: []string{"help"}, Flags: map[string]string{},
	}
	r, err := Dispatch(cmds, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Pretty, "mvctl") || !strings.Contains(r.Pretty, "status") {
		t.Fatal("help output missing commands")
	}
	if _, err := Dispatch(cmds, Context{Ctx: context.Background(), G: Global{}, C: ctx.C, Args: []string{"nope"}, Flags: map[string]string{}}); err == nil {
		t.Fatal("unknown command must error")
	}
}

func TestURLAndHealthCommands(t *testing.T) {
	c := NewClient(Global{BaseURL: "http://127.0.0.1:8471/", Timeout: time.Second})
	if c.base != "http://127.0.0.1:8471" {
		t.Fatalf("base URL not trimmed: %s", c.base)
	}
	ctx := Context{Ctx: context.Background(), G: Global{BaseURL: "http://127.0.0.1:8471"}, C: c, Flags: map[string]string{}}
	r, err := cmdURL(ctx)
	if err != nil || r.Pretty != "http://127.0.0.1:8471" {
		t.Fatalf("url command: %+v %v", r, err)
	}
}
