package web

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestDashFileAllowlist(t *testing.T) {
	ok := []string{
		DashPrefix + "css/dashboard.css",
		DashPrefix + "vendor/uplot.js",
		DashPrefix + "js/core.js",
	}
	for _, p := range ok {
		rel, ct, good := dashFile(p)
		if !good || rel == "" || ct == "" {
			t.Errorf("dashFile(%q) rejected, want allow", p)
		}
		if !dashExists(rel) {
			t.Errorf("embedded static/%s missing", rel)
		}
	}
	deny := []string{
		DashPrefix,
		strings.TrimSuffix(DashPrefix, "/"),
		DashPrefix + "../web.go",
		DashPrefix + "js/../../web.go",
		DashPrefix + "foo.txt",
		DashPrefix + "js/core.js/../../../passwd",
		DashPrefix + "js/",
		DashPrefix + "index.html",
		DashPrefix + "favicon.svg",
		"/index.html",
		"/css/dashboard.css",
	}
	for _, p := range deny {
		if _, _, good := dashFile(p); good {
			t.Errorf("dashFile(%q) allowed, want deny", p)
		}
	}
}

func TestServeDashKnownAndUnknown(t *testing.T) {
	core := DashPrefix + "js/core.js"
	req := httptest.NewRequest(http.MethodGet, core, nil)
	rec := httptest.NewRecorder()
	ServeDash(rec, req)
	if rec.Code != 200 {
		t.Fatalf("core.js → %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(body), "const dashCfg") {
		t.Fatalf("core.js body missing dashCfg")
	}

	req = httptest.NewRequest(http.MethodHead, core, nil)
	rec = httptest.NewRecorder()
	ServeDash(rec, req)
	if rec.Code != 200 {
		t.Fatalf("HEAD core.js → %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD leaked %d body bytes", rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Fatal("HEAD missing Content-Length")
	}

	req = httptest.NewRequest(http.MethodGet, DashPrefix+"nope.js", nil)
	rec = httptest.NewRecorder()
	ServeDash(rec, req)
	if rec.Code != 404 {
		t.Fatalf("unknown → %d, want 404", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, core, nil)
	rec = httptest.NewRecorder()
	ServeDash(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST → %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != staticAllow {
		t.Fatalf("Allow = %q, want %q", rec.Header().Get("Allow"), staticAllow)
	}
}

func TestIndexAndFaviconMethods(t *testing.T) {
	api := NewAggAPI(metrics.NewBuffer(16), nil, config.Default().StorageQueryTimeout)
	api.Dash = func() any {
		t.Fatal("method probe requested bootstrap state")
		return nil
	}
	rec := httptest.NewRecorder()
	Handler(api).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / → %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != staticAllow {
		t.Fatalf("POST / Allow = %q", rec.Header().Get("Allow"))
	}

	rec = httptest.NewRecorder()
	Handler(api).ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/", nil))
	if rec.Code != 200 {
		t.Fatalf("HEAD / → %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD / leaked %d body bytes", rec.Body.Len())
	}
	if rec.Header().Get("Content-Security-Policy") != "frame-ancestors 'none'" {
		t.Fatal("dashboard must deny framing")
	}
	if rec.Header().Get("ETag") != "" || rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("HEAD / must not cache dynamic bootstrap state")
	}

	rec = httptest.NewRecorder()
	Favicon().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("GET /favicon.ico → %d %s, want 200 image/svg+xml", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("<svg")) {
		t.Fatal("GET /favicon.ico body is not the brand SVG")
	}

	rec = httptest.NewRecorder()
	Favicon().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/favicon.ico", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /favicon.ico → %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != staticAllow {
		t.Fatalf("POST /favicon.ico Allow = %q, want %q", rec.Header().Get("Allow"), staticAllow)
	}
}

// TestDashCfgSeedsMatchDefault locks the first-paint JS seeds to
// config.Default() so they cannot drift into a second table. Runtime still
// overlays GET /admin/config (applyDashValues).
func TestDashCfgSeedsMatchDefault(t *testing.T) {
	b, err := staticFS.ReadFile("static/js/core.js")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`const dashCfg = \{([^}]+)\}`).FindSubmatch(b)
	if block == nil {
		t.Fatal("core.js missing dashCfg object")
	}
	intField := func(key string) int {
		t.Helper()
		m := regexp.MustCompile(key + `:\s*(\d+)`).FindSubmatch(block[1])
		if m == nil {
			t.Fatalf("dashCfg.%s missing", key)
		}
		n, err := strconv.Atoi(string(m[1]))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	d := config.Default()
	want := map[string]int{
		"history_size":      d.HistorySize,
		"log_rows":          d.DashLogRows,
		"poll_ms":           int(d.DashPollInterval.Milliseconds()),
		"chart_ms":          int(d.DashChartRefresh.Milliseconds()),
		"explorer_stale_ms": int(d.DashExplorerStale.Milliseconds()),
	}
	for key, w := range want {
		if got := intField(key); got != w {
			t.Errorf("dashCfg.%s = %d, want Default %d", key, got, w)
		}
	}
}

// TestCanonicalFieldFallbacksMatchOwners locks the dashboard's first-paint
// fallback lists and the ui_check fixture to their single Go owners, so a
// field added or removed on either side fails the build:
//   - chrome.js USAGE_FIELDS_FALLBACK == metrics.CanonicalUsageFields (the
//     canonical token fields, served by GET /admin/config as usage_fields)
//   - chrome.js MODEL_FIELDS_FALLBACK == format.CanonicalModelFields (the
//     /v1/models enrichment vocabulary, served as model_fields)
//   - ui_check.js cfgDoc.usage_fields == metrics.CanonicalUsageFields (the
//     harness stubs the /admin/config payload and asserts its dropdown)
func TestCanonicalFieldFallbacksMatchOwners(t *testing.T) {
	js, err := staticFS.ReadFile("static/js/chrome.js")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := jsStringList(t, js, "USAGE_FIELDS_FALLBACK = "), metrics.CanonicalUsageFields; !slices.Equal(got, want) {
		t.Errorf("chrome.js USAGE_FIELDS_FALLBACK = %v, want metrics.CanonicalUsageFields %v", got, want)
	}
	if got, want := jsStringList(t, js, "MODEL_FIELDS_FALLBACK = "), format.CanonicalModelFields; !slices.Equal(got, want) {
		t.Errorf("chrome.js MODEL_FIELDS_FALLBACK = %v, want format.CanonicalModelFields %v", got, want)
	}
	root := moduleRoot(t)
	ui, err := os.ReadFile(filepath.Join(root, "tests", "ui_check.js"))
	if err != nil {
		t.Fatalf("read ui_check.js: %v", err)
	}
	if got, want := jsStringList(t, ui, "usage_fields: "), metrics.CanonicalUsageFields; !slices.Equal(got, want) {
		t.Errorf("ui_check.js cfgDoc.usage_fields = %v, want metrics.CanonicalUsageFields %v", got, want)
	}
}

// jsStringList extracts a single-line string-array literal (decl + ['a',
// 'b', …]) from src, failing the test when the declaration is missing or
// empty. Only the canonical-field lists use this shape; a multi-line array
// would not match and the test fails loudly instead of guessing.
func jsStringList(t *testing.T, src []byte, decl string) []string {
	t.Helper()
	m := regexp.MustCompile(regexp.QuoteMeta(decl) + `\[([^\]]*)\]`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("%q array literal not found", decl)
	}
	quotes := regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(m[1], -1)
	out := make([]string, 0, len(quotes))
	for _, q := range quotes {
		out = append(out, string(q[1]))
	}
	if len(out) == 0 {
		t.Fatalf("%q array literal has no entries", decl)
	}
	return out
}

// moduleRoot locates the repo root from this file's compile-time source path
// (the same self-location rule cmd/proxy/restart.go applies). A -trimpath
// build cannot self-locate its source - the ui_check.js contract then skips
// with the reason instead of guessing a path.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok || file == "" {
		t.Skip("cannot locate own source path (trimpath build) - ui_check.js contract unchecked")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("no go.mod above " + dir + " - ui_check.js contract unchecked")
		}
		dir = parent
	}
}

var dashRefRe = regexp.MustCompile(`(?:href|src)="` + regexp.QuoteMeta(DashPrefix) + `([^"]+)"`)

func dashRelsFromIndex(html string) []string {
	ms := dashRefRe.FindAllStringSubmatch(html, -1)
	out := make([]string, 0, len(ms))
	seen := map[string]bool{}
	for _, m := range ms {
		rel := m[1]
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, rel)
	}
	return out
}

func TestIndexReferencesDashAssets(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	Handler(NewAggAPI(metrics.NewBuffer(16), nil, config.Default().StorageQueryTimeout)).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("index → %d", rec.Code)
	}
	html := rec.Body.String()
	headEnd := strings.Index(html, "</head>")
	for _, script := range regexp.MustCompile(`<script[^>]+src="[^"]+"[^>]*></script>`).FindAllStringIndex(html, -1) {
		if script[1] > headEnd || !strings.Contains(html[script[0]:script[1]], " defer ") {
			t.Error("external dashboard scripts must download early, with ordered deferred execution")
		}
	}
	if strings.Contains(html, "<script>\n// ---------- state ----------") {
		t.Error("index.html still inlines the app script - split failed")
	}

	rels := dashRelsFromIndex(html)
	if len(rels) == 0 {
		t.Fatalf("index.html has no %s assets", DashPrefix)
	}
	referenced := map[string]bool{}
	for _, rel := range rels {
		referenced[rel] = true
		if _, _, ok := dashFile(DashPrefix + rel); !ok {
			t.Errorf("index refs %s but dashFile rejects it", rel)
		}
		if !dashExists(rel) {
			t.Errorf("index refs missing embed static/%s", rel)
		}
	}

	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if _, allow := dashTypes[path.Ext(p)]; !allow {
			return nil
		}
		rel := strings.TrimPrefix(p, "static/")
		if !referenced[rel] {
			t.Errorf("embedded static/%s is not referenced from index.html", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestStaticETagRevalidation: static responses carry a weak ETag and
// revalidate to a body-less 304; a changed body (rebuild) changes the tag.
func TestStaticETagRevalidation(t *testing.T) {
	h := http.HandlerFunc(ServeDash)
	assetPath := DashPrefix + "css/dashboard.css"
	req := httptest.NewRequest(http.MethodGet, assetPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	etag := rec.Header().Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, `W/"`) {
		t.Fatalf("ETag = %q, want weak validator", etag)
	}
	if rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want Accept-Encoding (the gzip layer varies the bytes)", rec.Header().Get("Vary"))
	}

	req2 := httptest.NewRequest(http.MethodGet, assetPath, nil)
	req2.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match → %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Fatalf("304 leaked %d body bytes", rec2.Body.Len())
	}

	// A different asset's tag must not match (per-URL identity).
	core := httptest.NewRecorder()
	ServeDash(core, httptest.NewRequest(http.MethodGet, DashPrefix+"js/core.js", nil))
	if core.Header().Get("ETag") == etag {
		t.Fatal("dashboard.css and core.js share an ETag")
	}

	// List form + W/ prefix tolerance (browsers echo what we sent).
	req3 := httptest.NewRequest(http.MethodGet, assetPath, nil)
	req3.Header.Set("If-None-Match", `W/"bogus", `+etag)
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNotModified {
		t.Fatalf("list If-None-Match → %d, want 304", rec3.Code)
	}

	// A stale tag re-downloads.
	req4 := httptest.NewRequest(http.MethodGet, assetPath, nil)
	req4.Header.Set("If-None-Match", `W/"deadbeef"`)
	rec4 := httptest.NewRecorder()
	h.ServeHTTP(rec4, req4)
	if rec4.Code != 200 {
		t.Fatalf("stale If-None-Match → %d, want 200", rec4.Code)
	}
}

func TestServeDashRepresentations(t *testing.T) {
	for _, rel := range []string{"css/dashboard.css", "vendor/uplot.js"} {
		t.Run(rel, func(t *testing.T) {
			asset := staticCache["static/"+rel]
			for _, tc := range []struct {
				name, method, accept string
				conditional, gzip    bool
			}{
				{name: "gzip", method: http.MethodGet, accept: "gzip", gzip: true},
				{name: "fractional", method: http.MethodGet, accept: "br;q=1, gzip;q=0.5", gzip: true},
				{name: "identity", method: http.MethodGet},
				{name: "unsupported", method: http.MethodGet, accept: "br"},
				{name: "refused", method: http.MethodGet, accept: "gzip;q=0"},
				{name: "malformed", method: http.MethodGet, accept: "gzip;q=high"},
				{name: "empty qvalue", method: http.MethodGet, accept: "gzip;q="},
				{name: "NaN qvalue", method: http.MethodGet, accept: "gzip;q=NaN"},
				{name: "infinite qvalue", method: http.MethodGet, accept: "gzip;q=+Inf"},
				{name: "excessive qvalue", method: http.MethodGet, accept: "gzip;q=2"},
				{name: "negative qvalue", method: http.MethodGet, accept: "gzip;q=-0.5"},
				{name: "head gzip", method: http.MethodHead, accept: "gzip"},
				{name: "head identity", method: http.MethodHead},
				{name: "304 gzip", method: http.MethodGet, accept: "gzip", conditional: true},
				{name: "304 identity", method: http.MethodGet, conditional: true},
				{name: "304 head", method: http.MethodHead, accept: "gzip", conditional: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					req := httptest.NewRequest(tc.method, DashPrefix+rel, nil)
					req.Header.Set("Accept-Encoding", tc.accept)
					if tc.conditional {
						req.Header.Set("If-None-Match", asset.etag)
					}
					res := httptest.NewRecorder()
					ServeDash(res, req)
					if res.Header().Get("ETag") != asset.etag || res.Header().Get("Vary") != "Accept-Encoding" || res.Header().Get("Cache-Control") != "no-cache" {
						t.Fatalf("representation lost source validators: %v", res.Header())
					}
					wantEncoding := ""
					if tc.gzip {
						wantEncoding = "gzip"
					}
					if got := res.Header().Get("Content-Encoding"); got != wantEncoding {
						t.Fatalf("Content-Encoding=%q, want %q", got, wantEncoding)
					}
					if tc.conditional {
						if res.Code != http.StatusNotModified || res.Body.Len() != 0 {
							t.Fatalf("conditional response: code=%d bytes=%d", res.Code, res.Body.Len())
						}
						return
					}
					if res.Code != http.StatusOK {
						t.Fatalf("status=%d", res.Code)
					}
					if tc.method == http.MethodHead {
						if res.Body.Len() != 0 || res.Header().Get("Content-Length") != strconv.Itoa(len(asset.body)) {
							t.Fatal("HEAD must preserve identity length without a body")
						}
						return
					}
					body := res.Body.Bytes()
					if res.Header().Get("Content-Length") != strconv.Itoa(len(body)) {
						t.Fatal("Content-Length does not describe the selected representation")
					}
					if tc.gzip {
						zr, err := gzip.NewReader(res.Body)
						if err != nil {
							t.Fatal(err)
						}
						body, err = io.ReadAll(zr)
						if err != nil {
							t.Fatal(err)
						}
						if err := zr.Close(); err != nil {
							t.Fatal(err)
						}
					}
					if !slices.Equal(body, asset.body) {
						t.Fatal("static representation changed original bytes")
					}
				})
			}
		})
	}
}

func TestStaticCompressionKeepsSourceIdentity(t *testing.T) {
	for _, body := range [][]byte{[]byte("x"), []byte(strings.Repeat("compressible dashboard source", 100))} {
		asset := newStaticAsset(body)
		if asset.etag != `W/"`+etagHex(body)+`"` || !slices.Equal(asset.body, body) {
			t.Fatal("compressed representation changed source identity")
		}
		if len(asset.gzip) >= len(body) {
			t.Fatal("retained compression that increased the representation size")
		}
		req := httptest.NewRequest(http.MethodGet, "/asset", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		res := httptest.NewRecorder()
		writeStaticCached(res, req, "text/plain", "no-cache", asset)
		if len(asset.gzip) == 0 {
			if res.Header().Get("Content-Encoding") != "" || !slices.Equal(res.Body.Bytes(), body) {
				t.Fatal("small asset must use its shorter identity representation")
			}
		} else if res.Header().Get("Content-Encoding") != "gzip" || !slices.Equal(res.Body.Bytes(), asset.gzip) {
			t.Fatal("static GET did not use its cached compressed representation")
		}
	}
}

type staticBenchmarkWriter struct{ header http.Header }

func (w staticBenchmarkWriter) Header() http.Header         { return w.header }
func (w staticBenchmarkWriter) WriteHeader(int)             {}
func (w staticBenchmarkWriter) Write(p []byte) (int, error) { return len(p), nil }

// Fixed vendored bytes keep the delivery benchmark comparable while app JS
// evolves. Discarding output measures server work, not recorder body copies.
func BenchmarkServeDash(b *testing.B) {
	for _, tc := range []struct {
		name, method, accept string
		conditional          bool
	}{
		{name: "identity", method: http.MethodGet},
		{name: "gzip", method: http.MethodGet, accept: "gzip"},
		{name: "head", method: http.MethodHead, accept: "gzip"},
		{name: "not_modified", method: http.MethodGet, accept: "gzip", conditional: true},
		{name: "malformed_q", method: http.MethodGet, accept: "gzip;q=high"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			req := httptest.NewRequest(tc.method, DashPrefix+"vendor/uplot.js", nil)
			req.Header.Set("Accept-Encoding", tc.accept)
			if tc.conditional {
				req.Header.Set("If-None-Match", staticCache["static/vendor/uplot.js"].etag)
			}
			writer := staticBenchmarkWriter{header: make(http.Header)}
			handler := http.HandlerFunc(ServeDash)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				clear(writer.header)
				handler.ServeHTTP(writer, req)
			}
		})
	}
}

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
		req.Header.Set("If-None-Match", staticCache[dashboardIndexPath].etag)
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

func TestLoadStaticVersion(t *testing.T) {
	fixture := func() fstest.MapFS {
		return fstest.MapFS{
			dashboardIndexPath:   {Data: []byte(`<meta name="dashboard-version" content="__DASHBOARD_VERSION__"><script id="dashboard-bootstrap" type="application/json">__DASHBOARD_BOOTSTRAP__</script>`)},
			dashboardFaviconPath: {Data: []byte(`<svg/>`)},
			"static/css/app.css": {Data: []byte(`body{color:white}`)},
			"static/js/app.js":   {Data: []byte(`const ready=true;`)},
		}
	}
	base, version := loadStatic(fixture())
	rebuilt, rebuiltVersion := loadStatic(fixture())
	if version != rebuiltVersion || base[dashboardIndexPath].etag != rebuilt[dashboardIndexPath].etag {
		t.Fatal("identical asset rebuild changed the version or HTML validator")
	}
	if !strings.Contains(string(base[dashboardIndexPath].body), `content="`+version+`"`) || strings.Contains(string(base[dashboardIndexPath].body), dashboardVersionMarker) {
		t.Fatal("cached index did not replace its source marker with the asset version")
	}
	if strings.Count(string(base[dashboardIndexPath].body), dashboardBootstrapMarker) != 1 {
		t.Fatal("cached shell must retain its request-time state marker")
	}
	for _, changed := range []string{dashboardIndexPath, "static/css/app.css", "static/js/app.js", dashboardFaviconPath} {
		t.Run(changed, func(t *testing.T) {
			files := fixture()
			files[changed].Data = append(files[changed].Data, []byte("\nchanged")...)
			next, nextVersion := loadStatic(files)
			if nextVersion == version {
				t.Fatal("changed asset retained the dashboard version")
			}
			index := next[dashboardIndexPath]
			if index.etag == base[dashboardIndexPath].etag || !strings.Contains(string(index.body), `content="`+nextVersion+`"`) {
				t.Fatal("asset change did not update the served HTML version and ETag")
			}
			for p, old := range base {
				if p != changed && p != dashboardIndexPath && old.etag != next[p].etag {
					t.Errorf("unmodified %s lost its reusable validator", p)
				}
			}
		})
	}
	for _, change := range []string{"add", "remove", "rename"} {
		t.Run(change, func(t *testing.T) {
			files := fixture()
			switch change {
			case "add":
				files["static/js/new.js"] = &fstest.MapFile{Data: []byte(`const added=true;`)}
			case "remove":
				delete(files, "static/js/app.js")
			case "rename":
				files["static/js/new.js"] = files["static/js/app.js"]
				delete(files, "static/js/app.js")
			}
			_, nextVersion := loadStatic(files)
			if nextVersion == version {
				t.Fatal("changed asset set retained the dashboard version")
			}
		})
	}
}

type unreadableStaticFS struct{ fs.FS }

func (unreadableStaticFS) ReadFile(string) ([]byte, error) { return nil, fs.ErrPermission }

func TestLoadStaticFailsClosed(t *testing.T) {
	for name, fsys := range map[string]fs.FS{
		"missing tree": fstest.MapFS{},
		"missing index": fstest.MapFS{
			dashboardFaviconPath: {Data: []byte(`<svg/>`)},
		},
		"missing favicon": fstest.MapFS{
			dashboardIndexPath: {Data: []byte(dashboardVersionMarker)},
		},
		"missing marker": fstest.MapFS{
			dashboardIndexPath:   {Data: []byte(`<html/>`)},
			dashboardFaviconPath: {Data: []byte(`<svg/>`)},
		},
		"duplicate marker": fstest.MapFS{
			dashboardIndexPath:   {Data: []byte(dashboardVersionMarker + dashboardVersionMarker)},
			dashboardFaviconPath: {Data: []byte(`<svg/>`)},
		},
		"missing bootstrap marker": fstest.MapFS{
			dashboardIndexPath:   {Data: []byte(dashboardVersionMarker)},
			dashboardFaviconPath: {Data: []byte(`<svg/>`)},
		},
		"duplicate bootstrap marker": fstest.MapFS{
			dashboardIndexPath:   {Data: []byte(dashboardVersionMarker + dashboardBootstrapMarker + dashboardBootstrapMarker)},
			dashboardFaviconPath: {Data: []byte(`<svg/>`)},
		},
		"unreadable asset": unreadableStaticFS{fstest.MapFS{
			dashboardIndexPath: {Data: []byte(dashboardVersionMarker)},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("invalid embedded assets returned a partial cache")
				}
			}()
			loadStatic(fsys)
		})
	}
}

// TestGzipMiddleware: accepted encodings compress with Vary + ETag/304 intact;
// missing headers, HEAD, and 304 responses stay uncompressed.
func TestGzipMiddleware(t *testing.T) {
	body := strings.Repeat("millivolt dashboard payload ", 500) // ~5.5 KB compressible
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(body))
	})
	h := Gzip(next)

	get := func(accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if accept != "" {
			req.Header.Set("Accept-Encoding", accept)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := get("gzip")
	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body not gzip: %v", err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != body {
		t.Fatalf("decompressed body mismatch (%d vs %d bytes)", len(got), len(body))
	}

	// A fractional non-zero qvalue is acceptance (RFC 9110 §12.5.3) - the
	// old parser refused every q=0.x, so pin the fix.
	rec = get("br;q=1.0, gzip;q=0.5")
	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("gzip;q=0.5: Content-Encoding = %q, want gzip", enc)
	}

	for _, deny := range []string{"", "identity", "br", "gzip;q=0", "br;q=1.0, gzip;q=0.000", "gzip;q=high", "gzip;q=", "gzip;q=NaN", "gzip;q=+Inf", "gzip;q=-Inf", "gzip;q=2", "gzip;q=-0.5"} {
		rec := get(deny)
		if enc := rec.Header().Get("Content-Encoding"); enc != "" {
			t.Errorf("Accept-Encoding %q: Content-Encoding = %q, want none", deny, enc)
		}
		if rec.Body.String() != body {
			t.Errorf("Accept-Encoding %q: body wrong (%d bytes)", deny, rec.Body.Len())
		}
	}

	// HEAD never claims gzip (no body to describe).
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	recH := httptest.NewRecorder()
	h.ServeHTTP(recH, req)
	if recH.Header().Get("Content-Encoding") != "" {
		t.Fatal("HEAD answered with Content-Encoding: gzip")
	}

	// 304 flows through without an encoding claim.
	etagged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"x"`)
		if r.Header.Get("If-None-Match") == `W/"x"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte(body))
	})
	req304 := httptest.NewRequest(http.MethodGet, "/", nil)
	req304.Header.Set("Accept-Encoding", "gzip")
	req304.Header.Set("If-None-Match", `W/"x"`)
	rec304 := httptest.NewRecorder()
	Gzip(etagged).ServeHTTP(rec304, req304)
	if rec304.Code != http.StatusNotModified || rec304.Header().Get("Content-Encoding") != "" {
		t.Fatalf("304 under gzip: code=%d enc=%q", rec304.Code, rec304.Header().Get("Content-Encoding"))
	}
}

// Build the payload through the production HTML/bootstrap owners. Varied IDs,
// timestamps, metrics, metadata, and retries avoid an unrealistically repeated
// string fixture; no live/user history is read by this benchmark.
func benchmarkDashboardGzipBody(tb testing.TB) []byte {
	tb.Helper()
	d := config.Default()
	n := BootRingCapMul * d.DashLogRows
	buf := metrics.NewBuffer(n)
	rng := rand.New(rand.NewPCG(0x232745, 0x298671))
	start := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	for i := range n {
		at := start.Add(time.Duration(i)*time.Second + time.Duration(rng.IntN(1000))*time.Millisecond)
		rec := mkRec(fmt.Sprintf("%016x-%016x", rng.Uint64(), rng.Uint64()), at, 200, "", nil,
			int64(1+rng.IntN(4000)), int64(5000+rng.IntN(60000)), int64(rng.IntN(200000)), int64(rng.IntN(4000)), int64(rng.IntN(30000)), rng.Float64())
		rec.Provider, rec.Model, rec.Client = fmt.Sprintf("provider-%d.example", i%4), fmt.Sprintf("model-%d", i%12), fmt.Sprintf("client-%d", i%3)
		rec.KeyHash, rec.ConversationID = fmt.Sprintf("key-%064x", i%16), fmt.Sprintf("conversation-%032x", i/3)
		rec.UserAgent, rec.ClientIP = "NeutralSDK/1.0 (linux; amd64)", "127.0.0.1"
		rec.Path, rec.Method, rec.ClientLang = "/v1/chat/completions", "POST", "go"
		rec.ClientMeta = metrics.ClientMeta{OS: "linux", Arch: "amd64", Runtime: "go", RuntimeVer: "1.26", PkgVer: "1.0", Format: "chat"}
		rec.ProviderRequestID, rec.ProviderServer = fmt.Sprintf("%016x", rng.Uint64()), "neutral-gateway"
		rec.ProcessingMs, rec.Stream = rng.IntN(1000), true
		rec.FirstTokenAt, rec.LastTokenAt = at.Add(time.Duration(rec.TTFTMs)*time.Millisecond), rec.End
		rec.FinalAttemptAt, rec.FirstAnswerAt = at, rec.FirstTokenAt.Add(time.Millisecond)
		rec.FinishReason = "stop"
		rec.OverallTPS, rec.DecodeTPS = rng.Float64()*120, rng.Float64()*200
		rec.Usage.ReasoningTokens = int64(rng.IntN(1000))
		rec.Usage.TotalTokens = rec.Usage.InputTokens + rec.Usage.OutputTokens
		rec.AnswerTokens, rec.GenTokens = rec.Usage.OutputTokens-rec.Usage.ReasoningTokens, rec.Usage.OutputTokens
		rec.ReqToolsCount, rec.ReqToolChoice, rec.ReqStreamOpts = 5, "auto", true
		rec.TurnsUser, rec.TurnsAssistant, rec.TurnsTool = i%32+1, i%31, i%12
		rec.CharsSystem, rec.CharsUser, rec.CharsAssistant, rec.CharsTool = 4096, rng.IntN(100000), rng.IntN(100000), rng.IntN(10000)
		rec.LastTurnRole = "user"
		if i%7 == 0 {
			rec.Retries = 1
			rec.Attempts = []metrics.RetryAttempt{{StatusCode: 503, ErrorType: "server_error", ErrorMsg: "Upstream capacity is temporarily unavailable; retry this request.", At: at.Add(-time.Second)}}
		}
		buf.Record(rec)
	}
	api := NewAggAPI(buf, nil, d.StorageQueryTimeout)
	response := httptest.NewRecorder()
	Handler(api).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		tb.Fatalf("dashboard fixture status=%d", response.Code)
	}
	return response.Body.Bytes()
}

// Reset reuses the same writer workspace as the production gzip pool. The
// body and output buffer are prepared before timing: this isolates compression
// CPU and wire size from snapshot building, JSON marshaling, and networking.
func BenchmarkDynamicDashboardGzip(b *testing.B) {
	body := benchmarkDashboardGzipBody(b)
	if fixture := os.Getenv("MILLIVOLT_GZIP_BENCH_FIXTURE"); fixture != "" {
		var err error
		body, err = os.ReadFile(fixture)
		if err != nil {
			b.Fatal(err)
		}
	}
	for _, level := range []struct {
		name  string
		value int
	}{{"default", gzip.DefaultCompression}, {"best_speed", gzip.BestSpeed}, {"level_2", 2}, {"level_3", 3}} {
		b.Run(level.name, func(b *testing.B) {
			var encoded bytes.Buffer
			writer, err := gzip.NewWriterLevel(&encoded, level.value)
			if err != nil {
				b.Fatal(err)
			}
			compress := func() {
				encoded.Reset()
				writer.Reset(&encoded)
				if _, err := writer.Write(body); err != nil {
					b.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					b.Fatal(err)
				}
			}
			compress()
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for b.Loop() {
				compress()
			}
			b.ReportMetric(float64(encoded.Len()), "compressed-bytes/op")
			b.ReportMetric(float64(len(body)), "raw-bytes/op")
		})
	}
}
