package web

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// A rendered page must pin its own asset version before the first bootstrap
// arrives, including when a rebuild happens between those two requests.
func TestDashboardVersionMetadata(t *testing.T) {
	api := NewAggAPI(metrics.NewBuffer(16), nil, config.Default().StorageQueryTimeout)
	index := httptest.NewRecorder()
	Handler(api).ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	match := regexp.MustCompile(`<meta name="dashboard-version" content="([0-9a-f]{16})">`).FindStringSubmatch(index.Body.String())
	if len(match) != 2 {
		t.Fatal("served index missing a pinned dashboard asset version")
	}
	bootstrap := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.HandleBootstrap(w, r)
		if cache := w.Header().Get("Cache-Control"); cache != "no-store" {
			t.Errorf("bootstrap Cache-Control=%q, want no-store", cache)
		}
	})
	p := get(t, bootstrap, "/metrics/bootstrap")
	if p["dashboard_version"] != match[1] {
		t.Fatalf("bootstrap dashboard_version=%v, rendered index=%s", p["dashboard_version"], match[1])
	}
}

func bootstrapFromHTML(t *testing.T, body string) map[string]any {
	t.Helper()
	matches := regexp.MustCompile(`<script id="dashboard-bootstrap" type="application/json">([\s\S]*?)</script>`).FindAllStringSubmatch(body, -1)
	if len(matches) != 1 {
		t.Fatalf("HTML has %d bootstrap data blocks, want exactly one", len(matches))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(matches[0][1]), &payload); err != nil {
		t.Fatalf("embedded bootstrap is not JSON: %v", err)
	}
	return payload
}

func TestDashboardBootstrapMatchesEndpoint(t *testing.T) {
	buf := metrics.NewBuffer(16)
	now := time.Now()
	buf.Record(mkRec("older", now.Add(-time.Minute), 200, "", nil, 10, 100, 20, 5, 0, .01))
	buf.Record(mkRec("newer", now.Add(-time.Second), 200, "", nil, 10, 100, 20, 5, 0, .02))
	buf.SetSnapshotLimit(1)
	buf.PublishLive("begin", &metrics.Record{ID: "pending", Start: now, Stream: true})
	api := NewAggAPI(buf, nil, config.Default().StorageQueryTimeout)
	api.Dash = func() any { return map[string]any{"log_rows": 50} }
	api.Pause = func() any { return map[string]any{"paused": true} }
	api.Throttle = func() any { return map[string]any{"limited": true} }
	api.Debug = func() any { return map[string]any{"active": true} }
	api.Storm = func() any {
		return map[string]any{"enabled": true, "banner_enabled": true, "storms": []any{map[string]any{"provider": "neutral.example", "queued": 3}}}
	}
	api.ModelCanon = func() config.ModelCanon { return config.Default().ModelCanon() }
	handler := Handler(api)
	read := func(target string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		// Browsers may still hold the old, static shell's validator. It must
		// never turn a state-bearing response into a stale or empty 304.
		req.Header.Set("If-None-Match", staticForTest(dashboardIndexPath).etag)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Header().Get("Content-Security-Policy") != "frame-ancestors 'none'" {
			t.Fatal("dashboard GET/HEAD must deny framing")
		}
		if res.Code != http.StatusOK || res.Header().Get("ETag") != "" || res.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("dynamic HTML status=%d headers=%v", res.Code, res.Header())
		}
		if res.Header().Get("Content-Length") != strconv.Itoa(res.Body.Len()) {
			t.Fatal("dynamic HTML Content-Length does not describe the inserted state")
		}
		return bootstrapFromHTML(t, res.Body.String())
	}
	// A new document never accepts a cursor from its URL: it needs a full
	// capped snapshot even when the URL contains live-endpoint parameters.
	payload := read("/?since=2&feed=" + buf.FeedID())
	want := get(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if !reflect.DeepEqual(payload, want) {
		t.Fatalf("HTML/bootstrap payloads differ: HTML=%v endpoint=%v", payload, want)
	}
	if payload["incremental"] != false || len(payload["records"].([]any)) != 1 || len(payload["in_flight_records"].([]any)) != 1 {
		t.Fatalf("initial page lost capped records or pending lifecycle state: %v", payload)
	}
	buf.Record(mkRec("latest", now, 200, "", nil, 10, 100, 20, 5, 0, .03))
	next := read("/index.html")
	if next["seq"] != float64(3) || next["records"].([]any)[0].(map[string]any)["id"] != "latest" {
		t.Fatalf("repeat navigation reused old embedded state: %v", next)
	}
	if next["dashboard_version"] != payload["dashboard_version"] {
		t.Fatal("live records changed the immutable frontend content version")
	}
}

func TestDashboardBootstrapEscapesUntrustedText(t *testing.T) {
	attack := "</script><script id=attack>alert(1)</script><!--<script>&\u2028\u2029"
	buf := metrics.NewBuffer(16)
	rec := mkRec("escaped", time.Now(), 500, "", nil, 0, 0, 0, 0, 0, 0)
	rec.ErrorMsg, rec.Provider, rec.Client = attack, attack, attack
	buf.Record(rec)
	api := NewAggAPI(buf, nil, config.Default().StorageQueryTimeout)
	api.Dash = func() any { return map[string]any{attack: attack} }
	api.Storm = func() any {
		return map[string]any{"storms": []any{map[string]any{"provider": attack, "model": attack}}}
	}
	res := httptest.NewRecorder()
	Handler(api).ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("HTML status=%d", res.Code)
	}
	body := res.Body.String()
	if strings.Contains(body, attack) || strings.Contains(body, "<script id=attack>") || strings.Contains(body, "<!--<script>") {
		t.Fatal("provider text escaped the inert JSON script")
	}
	for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`, `\u2028`, `\u2029`} {
		if !strings.Contains(body, escaped) {
			t.Errorf("HTML missing required JSON escape %s", escaped)
		}
	}
	payload := bootstrapFromHTML(t, body)
	if payload["records"].([]any)[0].(map[string]any)["error_msg"] != attack || payload["dash"].(map[string]any)[attack] != attack {
		t.Fatal("HTML escaping changed the original bootstrap data")
	}
	storm := payload["storm"].(map[string]any)["storms"].([]any)[0].(map[string]any)
	if storm["provider"] != attack || storm["model"] != attack {
		t.Fatal("HTML escaping changed the original storm scope")
	}
}

func TestDashboardBootstrapMarshalFailure(t *testing.T) {
	api := NewAggAPI(metrics.NewBuffer(16), nil, config.Default().StorageQueryTimeout)
	api.Dash = func() any { return math.Inf(1) }
	res := httptest.NewRecorder()
	Handler(api).ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/", nil))
	if res.Code != http.StatusInternalServerError || strings.Contains(res.Body.String(), "<html") || strings.Contains(res.Body.String(), "dashboard-bootstrap") {
		t.Fatalf("invalid state leaked partial HTML: status=%d body=%q", res.Code, res.Body.String())
	}
}
