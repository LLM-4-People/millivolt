package web

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
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
		DashPrefix + "js/settings.js",
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

	// The version query is request metadata, not a lookup path segment: an
	// unknown path stays 404 even with the current version attached.
	req = httptest.NewRequest(http.MethodGet, DashPrefix+"nope.js?v="+dashboardContentVersion(), nil)
	rec = httptest.NewRecorder()
	ServeDash(rec, req)
	if rec.Code != 404 {
		t.Fatalf("versioned unknown → %d, want 404", rec.Code)
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
	Brand("/favicon.ico").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/x-icon" {
		t.Fatalf("GET /favicon.ico → %d %s, want 200 image/x-icon", rec.Code, rec.Header().Get("Content-Type"))
	}
	if ico := rec.Body.Bytes(); len(ico) < 6 || ico[0] != 0 || ico[1] != 0 || ico[2] != 1 || ico[3] != 0 {
		t.Fatal("GET /favicon.ico body is not an ICO")
	}

	rec = httptest.NewRecorder()
	Brand("/favicon.svg").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/favicon.svg", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/svg+xml" || !bytes.Contains(rec.Body.Bytes(), []byte("<svg")) {
		t.Fatalf("GET /favicon.svg → %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}

	rec = httptest.NewRecorder()
	Brand("/manifest.webmanifest").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/manifest+json" {
		t.Fatalf("GET /manifest.webmanifest → %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	var manifest map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	if manifest["start_url"] != "/" || manifest["display"] != "standalone" {
		t.Fatalf("manifest missing install fields: %v", manifest)
	}
	assertChromiumInstallManifest(t, rec.Body.Bytes())

	rec = httptest.NewRecorder()
	Brand("/sw.js").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sw.js", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Service-Worker-Allowed") != "/" {
		t.Fatalf("GET /sw.js → %d SW-Allowed %q", rec.Code, rec.Header().Get("Service-Worker-Allowed"))
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("addEventListener('fetch'")) && !bytes.Contains(rec.Body.Bytes(), []byte(`addEventListener("fetch"`)) {
		t.Fatal("service worker has no fetch handler")
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(dashboardVersionMarker)) {
		t.Fatal("service worker still contains the unstamped version marker")
	}

	rec = httptest.NewRecorder()
	Brand("/favicon.ico").ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/favicon.ico", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /favicon.ico → %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != staticAllow {
		t.Fatalf("POST /favicon.ico Allow = %q, want %q", rec.Header().Get("Allow"), staticAllow)
	}
}

// TestPWAInstallability locks Chromium/iOS install surface: HTTPS-capable
// manifest members, 192/512 any+maskable PNGs at the declared sizes, ICO
// magic, apple-touch 180, start_url fetch coverage without caching live HTML.
func TestPWAInstallability(t *testing.T) {
	rec := httptest.NewRecorder()
	Brand("/manifest.webmanifest").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest → %d", rec.Code)
	}
	assertChromiumInstallManifest(t, rec.Body.Bytes())

	for _, spec := range []struct {
		path string
		w, h int
	}{
		{"/apple-touch-icon.png", 180, 180},
	} {
		rec = httptest.NewRecorder()
		Brand(spec.path).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, spec.path, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("%s → %d %s", spec.path, rec.Code, rec.Header().Get("Content-Type"))
		}
		img, _, err := image.Decode(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("%s decode: %v", spec.path, err)
		}
		b := img.Bounds()
		if b.Dx() != spec.w || b.Dy() != spec.h {
			t.Errorf("%s is %dx%d, want %dx%d", spec.path, b.Dx(), b.Dy(), spec.w, spec.h)
		}
	}

	index := httptest.NewRecorder()
	Handler(NewAggAPI(metrics.NewBuffer(16), nil, config.Default().StorageQueryTimeout)).ServeHTTP(
		index, httptest.NewRequest(http.MethodGet, "/", nil))
	html := index.Body.String()
	for _, needle := range []string{
		`rel="manifest" href="/manifest.webmanifest"`,
		`rel="apple-touch-icon" href="/apple-touch-icon.png"`,
		`name="apple-mobile-web-app-capable" content="yes"`,
		`name="mobile-web-app-capable" content="yes"`,
		`name="theme-color" content="#1b1826"`,
		`viewport-fit=cover`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("index.html missing %q", needle)
		}
	}
	live, err := staticFS.ReadFile("static/js/live.js")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(live, []byte("navigator.serviceWorker.register('/sw.js'")) {
		t.Fatal("live.js does not register /sw.js")
	}

	rec = httptest.NewRecorder()
	Brand("/sw.js").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sw.js", nil))
	sw := rec.Body.String()
	if rec.Header().Get("Service-Worker-Allowed") != "/" {
		t.Fatal("sw.js must declare Service-Worker-Allowed: /")
	}
	if !strings.Contains(sw, "addEventListener('fetch'") {
		t.Fatal("service worker has no fetch handler")
	}
	if !strings.Contains(sw, "req.mode === 'navigate'") || !strings.Contains(sw, "offlinePage") {
		t.Fatal("service worker must cover start_url navigations without caching live HTML")
	}
	liveFn, ok := jsFunctionBody(sw, "livePath")
	if !ok {
		t.Fatal("service worker missing livePath function")
	}
	for _, needle := range []string{
		"pathname === '/admin'",
		"pathname.startsWith('/admin/')",
		"pathname === '/session'",
		"pathname.startsWith('/session/')",
		"pathname === '/metrics'",
		"pathname.startsWith('/metrics/')",
		"pathname === '/v1'",
		"pathname.startsWith('/v1/')",
	} {
		if !strings.Contains(liveFn, needle) {
			t.Errorf("livePath function missing %s", needle)
		}
	}
	start := strings.Index(sw, "const SHELL_ASSETS = [")
	end := strings.Index(sw[start+1:], "];")
	if start < 0 || end < 0 {
		t.Fatal("service worker missing SHELL_ASSETS list")
	}
	shell := sw[start : start+1+end]
	if strings.Contains(shell, "'/'") || strings.Contains(shell, `"/"`) {
		t.Fatal("service worker must not precache gated /")
	}
	// The offline fallback page is authored inside offlinePage() as raw
	// string data. Its palette mirrors the dashboard's :root tokens
	// (#1b1826 = --well-lo, #141210 = --bg, #e8e4de = --text); these pins
	// catch a palette drift that a stylesheet can no longer equalize.
	for _, needle := range []string{
		`<meta name="theme-color" content="#1b1826">`,
		"background:#141210",
	} {
		if !strings.Contains(sw, needle) {
			t.Errorf("service worker offline page missing %s", needle)
		}
	}
}

// TestBrandIconCorners locks the install-icon corner contract: any-purpose
// icons keep transparent corners for rounded launchers, while maskable and
// Apple icons fill every corner with the canonical dark well so no white
// square appears after masking. The expected well comes from dashboard.css's
// --well-lo token, never a second palette table.
func TestBrandIconCorners(t *testing.T) {
	css, err := staticFS.ReadFile("static/css/dashboard.css")
	if err != nil {
		t.Fatal(err)
	}
	well := brandTokenColor(t, css, "--well-lo")
	cases := []struct {
		name        string
		transparent bool
	}{
		{"icon-192.png", true},
		{"icon-512.png", true},
		{"icon-192-maskable.png", false},
		{"icon-512-maskable.png", false},
		{"apple-touch-icon.png", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := staticFS.ReadFile("static/" + tc.name)
			if err != nil {
				t.Fatal(err)
			}
			img, err := png.Decode(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			b := img.Bounds()
			for _, point := range []image.Point{
				{X: b.Min.X, Y: b.Min.Y},
				{X: b.Max.X - 1, Y: b.Min.Y},
				{X: b.Min.X, Y: b.Max.Y - 1},
				{X: b.Max.X - 1, Y: b.Max.Y - 1},
			} {
				r, g, bl, a := img.At(point.X, point.Y).RGBA()
				if tc.transparent {
					if a != 0 {
						t.Errorf("corner %v alpha=%d, want fully transparent", point, a)
					}
					continue
				}
				if a != 0xffff {
					t.Errorf("corner %v alpha=%d, want opaque", point, a)
					continue
				}
				got := color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(bl >> 8), A: 255}
				if r>>8 >= 0xf0 && g>>8 >= 0xf0 && bl>>8 >= 0xf0 {
					t.Errorf("corner %v is opaque near-white %v", point, got)
				}
				if got != well {
					t.Errorf("corner %v = %v, want canonical --well-lo %v", point, got, well)
				}
			}
		})
	}
}

func brandTokenColor(t *testing.T, css []byte, token string) color.RGBA {
	t.Helper()
	match := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(token) + `\s*:\s*#([0-9a-fA-F]{6})\s*;`).
		FindSubmatch(css)
	if match == nil {
		t.Fatalf("dashboard.css has no %s hex token", token)
	}
	value, err := strconv.ParseUint(string(match[1]), 16, 32)
	if err != nil {
		t.Fatalf("%s value: %v", token, err)
	}
	return color.RGBA{R: uint8(value >> 16), G: uint8(value >> 8), B: uint8(value), A: 255}
}

func assertChromiumInstallManifest(t *testing.T, raw []byte) {
	t.Helper()
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	if manifest["name"] != "millivolt" || manifest["short_name"] != "millivolt" {
		t.Fatalf("manifest name: %v", manifest)
	}
	if manifest["start_url"] != "/" || manifest["scope"] != "/" || manifest["id"] != "/" {
		t.Fatalf("manifest identity: %v", manifest)
	}
	if manifest["display"] != "standalone" {
		t.Fatalf("manifest display=%v", manifest["display"])
	}
	if v, ok := manifest["prefer_related_applications"]; ok && v != false {
		t.Fatalf("prefer_related_applications=%v, want false or absent", v)
	}
	if manifest["theme_color"] != "#1b1826" || manifest["background_color"] != "#1b1826" {
		t.Fatalf("manifest colors: %v", manifest)
	}
	icons, _ := manifest["icons"].([]any)
	need := map[string]bool{
		"192x192 any":      false,
		"512x512 any":      false,
		"192x192 maskable": false,
		"512x512 maskable": false,
	}
	brand := map[string]bool{}
	for _, p := range BrandPaths() {
		brand[p] = true
	}
	for _, rawIcon := range icons {
		icon, _ := rawIcon.(map[string]any)
		src, _ := icon["src"].(string)
		sizes, _ := icon["sizes"].(string)
		purpose, _ := icon["purpose"].(string)
		typ, _ := icon["type"].(string)
		if !brand[src] {
			t.Errorf("manifest icon src %q is not an ungated Brand path", src)
			continue
		}
		if typ != "image/png" {
			t.Errorf("icon %s type=%q, want image/png", src, typ)
		}
		if strings.Contains(purpose, " ") {
			t.Errorf("icon %s purpose=%q must be a single token (any and maskable are separate files)", src, purpose)
		}
		var w, h int
		if _, err := fmt.Sscanf(sizes, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
			t.Errorf("icon %s sizes=%q", src, sizes)
			continue
		}
		rec := httptest.NewRecorder()
		Brand(src).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, src, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
			t.Errorf("Brand(%s) → %d %s", src, rec.Code, rec.Header().Get("Content-Type"))
			continue
		}
		img, _, err := image.Decode(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Errorf("%s decode: %v", src, err)
			continue
		}
		b := img.Bounds()
		if b.Dx() != w || b.Dy() != h {
			t.Errorf("%s is %dx%d, want %s", src, b.Dx(), b.Dy(), sizes)
		}
		key := sizes + " " + purpose
		if _, ok := need[key]; ok {
			need[key] = true
		}
	}
	for key, ok := range need {
		if !ok {
			t.Errorf("manifest missing %s icon", key)
		}
	}
}

func jsFunctionBody(src, name string) (string, bool) {
	sig := "function " + name + "("
	i := strings.Index(src, sig)
	if i < 0 {
		return "", false
	}
	brace := strings.Index(src[i:], "{")
	if brace < 0 {
		return "", false
	}
	start := i + brace
	depth := 0
	for j := start; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : j+1], true
			}
		}
	}
	return "", false
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
		// Word-boundary anchored like boolField: a longer key ending in this
		// one (dash_poll_ms vs poll_ms) must not shadow it.
		m := regexp.MustCompile(`\b` + key + `\s*:\s*(\d+)`).FindSubmatch(block[1])
		if m == nil {
			t.Fatalf("dashCfg.%s missing", key)
		}
		n, err := strconv.Atoi(string(m[1]))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	boolField := func(key string) bool {
		t.Helper()
		// Word-boundary anchored: a longer key ending in this one
		// (dash_background_refresh vs background_refresh) must not shadow it.
		m := regexp.MustCompile(`\b` + key + `\s*:\s*(true|false)`).FindSubmatch(block[1])
		if m == nil {
			t.Fatalf("dashCfg.%s missing", key)
		}
		return string(m[1]) == "true"
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
	if got := boolField("background_refresh"); got != d.DashBackgroundRefresh {
		t.Errorf("dashCfg.background_refresh = %v, want Default %v", got, d.DashBackgroundRefresh)
	}
}

// TestCanonicalFieldFallbacksMatchOwners locks the dashboard's settings
// fallback lists and the ui_check fixture to their single Go owners, so a
// field added or removed on either side fails the build:
//   - settings.js USAGE_FIELDS_FALLBACK == metrics.CanonicalUsageFields (the
//     canonical token fields, served by GET /admin/config as usage_fields)
//   - settings.js MODEL_FIELDS_FALLBACK == format.CanonicalModelFields (the
//     /v1/models enrichment vocabulary, served as model_fields)
//   - ui_check.js cfgDoc.usage_fields == metrics.CanonicalUsageFields (the
//     harness stubs the /admin/config payload and asserts its dropdown)
func TestCanonicalFieldFallbacksMatchOwners(t *testing.T) {
	js, err := staticFS.ReadFile("static/js/settings.js")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := jsStringList(t, js, "USAGE_FIELDS_FALLBACK = "), metrics.CanonicalUsageFields; !slices.Equal(got, want) {
		t.Errorf("settings.js USAGE_FIELDS_FALLBACK = %v, want metrics.CanonicalUsageFields %v", got, want)
	}
	if got, want := jsStringList(t, js, "MODEL_FIELDS_FALLBACK = "), format.CanonicalModelFields; !slices.Equal(got, want) {
		t.Errorf("settings.js MODEL_FIELDS_FALLBACK = %v, want format.CanonicalModelFields %v", got, want)
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
// (the same self-location rule cmd/proxy/restart.go applies). A stripped
// source view (for example a -trimpath build) cannot self-locate its source,
// so the ui_check.js contract fails with that reason instead of skipping or
// guessing a path.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok || file == "" {
		t.Fatalf("cannot locate own source path (source paths stripped, e.g. -trimpath): ui_check.js contract cannot be verified")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s: ui_check.js contract cannot be verified without the repository source view", dir)
		}
		dir = parent
	}
}

var dashRefRe = regexp.MustCompile(`(?:href|src)="` + regexp.QuoteMeta(DashPrefix) + `([^"]+)"`)

// dashAssetRef is one local /dash/ reference in the shell: the embedded
// relative path plus the raw query it carries. The version query is request
// metadata, never part of the asset lookup path.
type dashAssetRef struct{ rel, query string }

func dashRefsFromIndex(html string) []dashAssetRef {
	ms := dashRefRe.FindAllStringSubmatch(html, -1)
	out := make([]dashAssetRef, 0, len(ms))
	seen := map[string]bool{}
	for _, m := range ms {
		rel, query, _ := strings.Cut(m[1], "?")
		if seen[rel+"?"+query] {
			continue
		}
		seen[rel+"?"+query] = true
		out = append(out, dashAssetRef{rel: rel, query: query})
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

	rels := dashRefsFromIndex(html)
	if len(rels) == 0 {
		t.Fatalf("index.html has no %s assets", DashPrefix)
	}
	version := dashboardContentVersion()
	referenced := map[string]bool{}
	for _, ref := range rels {
		referenced[ref.rel] = true
		if ref.query != "v="+version {
			t.Errorf("index refs %s%s?%s, want exactly ?v=%s", DashPrefix, ref.rel, ref.query, version)
		}
		if _, _, ok := dashFile(DashPrefix + ref.rel); !ok {
			t.Errorf("index refs %s but dashFile rejects it", ref.rel)
		}
		if !dashExists(ref.rel) {
			t.Errorf("index refs missing embed static/%s", ref.rel)
		}
		rec := httptest.NewRecorder()
		ServeDash(rec, httptest.NewRequest(http.MethodGet, DashPrefix+ref.rel+"?"+ref.query, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("versioned %s → %d, want 200", ref.rel, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != dashImmutableCacheControl {
			t.Errorf("versioned %s: Cache-Control = %q, want immutable", ref.rel, cc)
		}
	}
	if meta := `<meta name="dashboard-version" content="` + version + `">`; strings.Count(html, meta) != 1 || strings.Contains(html, dashboardVersionMarker) {
		t.Fatal("served index must carry exactly one stamped dashboard version meta and no unstamped marker")
	}

	// settings.js is the one lazy shell asset: the Settings trigger injects
	// it with the meta version on first open, so the first-paint index must
	// not carry it and the loader must name the versioned URL.
	if referenced["js/settings.js"] {
		t.Error("index.html must not load settings.js on first paint")
	}
	chrome, err := staticFS.ReadFile("static/js/chrome.js")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(chrome, []byte(`'/dash/js/settings.js?v='`)) {
		t.Error("chrome.js does not name the versioned settings.js trigger URL")
	}
	if !dashExists("js/settings.js") {
		t.Error("embedded static/js/settings.js missing")
	}

	err = fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
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
		if _, brand := brandByURL["/"+rel]; brand {
			return nil
		}
		if rel == "js/settings.js" {
			return nil // lazy: injected by the Settings trigger, never first paint
		}
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

// TestStaticFreshnessContract pins the cache-busting story end to end.
// Versionless and stale-version /dash URLs revalidate on each load
// (no-cache + ETag) so an old index cannot pin new bytes under an old URL;
// only a URL carrying the process's exact content version is immutable.
// The served service worker stamps a content-identity cache name so a
// rebuild rotates the offline shell. A served marker, or a caching
// directive that lets online clients keep stale bytes, is exactly the
// staleness bug class this guards.
func TestStaticFreshnessContract(t *testing.T) {
	const immutable = dashImmutableCacheControl
	version := dashboardContentVersion()
	for _, rel := range []string{"js/chart.js", "js/settings.js", "css/dashboard.css", "vendor/uplot.js"} {
		assetPath := DashPrefix + rel

		rec := httptest.NewRecorder()
		ServeDash(rec, httptest.NewRequest(http.MethodGet, assetPath, nil))
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s: unversioned Cache-Control = %q, want no-cache (revalidate every load)", rel, cc)
		}
		etag := rec.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("%s: missing ETag to revalidate against", rel)
		}
		req := httptest.NewRequest(http.MethodGet, assetPath, nil)
		req.Header.Set("If-None-Match", etag)
		rec = httptest.NewRecorder()
		ServeDash(rec, req)
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Fatalf("%s: unversioned If-None-Match → %d with %d bytes, want body-less 304", rel, rec.Code, rec.Body.Len())
		}

		// The shell's exact version is immutable, and the representation's
		// validators, encoding and conditional behavior stay intact.
		versioned := assetPath + "?v=" + version
		req = httptest.NewRequest(http.MethodGet, versioned, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec = httptest.NewRecorder()
		ServeDash(rec, req)
		if cc := rec.Header().Get("Cache-Control"); cc != immutable {
			t.Fatalf("%s: versioned Cache-Control = %q, want %q", rel, cc, immutable)
		}
		if rec.Header().Get("ETag") != etag || rec.Header().Get("Vary") != "Accept-Encoding" {
			t.Fatalf("%s: versioned response lost its validators: %v", rel, rec.Header())
		}
		if rec.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("%s: versioned response dropped the negotiated gzip body", rel)
		}
		req = httptest.NewRequest(http.MethodGet, versioned, nil)
		req.Header.Set("If-None-Match", etag)
		rec = httptest.NewRecorder()
		ServeDash(rec, req)
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Fatalf("%s: versioned If-None-Match → %d with %d bytes, want body-less 304", rel, rec.Code, rec.Body.Len())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != immutable {
			t.Fatalf("%s: versioned 304 Cache-Control = %q, want %q", rel, cc, immutable)
		}

		// A stale version must never be immutable: the URL does not describe
		// the bytes this process now serves.
		req = httptest.NewRequest(http.MethodGet, assetPath+"?v=old", nil)
		rec = httptest.NewRecorder()
		ServeDash(rec, req)
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s: stale-version Cache-Control = %q, want no-cache", rel, cc)
		}
		if rec.Header().Get("ETag") == "" {
			t.Fatalf("%s: stale-version response lost its revalidation tag", rel)
		}
	}
	rec := httptest.NewRecorder()
	Brand("/sw.js").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sw.js", nil))
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("served sw.js Cache-Control = %q, want no-cache", cc)
	}
	for _, brandPath := range []string{"/favicon.svg", "/manifest.webmanifest"} {
		brandRec := httptest.NewRecorder()
		Brand(brandPath).ServeHTTP(brandRec, httptest.NewRequest(http.MethodGet, brandPath, nil))
		if cc := brandRec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s Cache-Control = %q, want no-cache", brandPath, cc)
		}
	}
	body := rec.Body.String()
	if strings.Contains(body, dashboardVersionMarker) {
		t.Fatal("served sw.js still contains the unstamped __DASHBOARD_VERSION__ marker")
	}
	if !strings.Contains(body, "millivolt-shell-") {
		t.Fatal("served sw.js does not name its shell cache")
	}
	// sw.js builds its cache name from the prefix const plus the stamped
	// dashboard version; parse that concatenation for the content identity.
	cacheName := regexp.MustCompile(`CACHE = CACHE_PREFIX \+ '([^']+)'`).FindStringSubmatch(body)
	if cacheName == nil {
		t.Fatal("served sw.js does not build its shell cache from the stamped version")
	}
	version = cacheName[1]
	if len(version) < 8 {
		t.Fatalf("served sw.js cache version = %q, want a content identity hash", version)
	}
	// The stamped cache identity must equal the process's dashboard version
	// (the single source of truth loadStatic computes from every asset).
	if version != dashboardContentVersion() {
		t.Fatalf("sw.js cache identity %q != dashboardContentVersion() %q", version, dashboardContentVersion())
	}
}

// TestDashPreservesGateNoStore pins the gate's stricter policy: a Bearer
// request that mints a session cookie sets no-store before ServeDash runs.
// The versioned immutable policy must never override it on that response.
func TestDashPreservesGateNoStore(t *testing.T) {
	version := dashboardContentVersion()
	rec := httptest.NewRecorder()
	rec.Header().Set("Cache-Control", "no-store")
	ServeDash(rec, httptest.NewRequest(http.MethodGet,
		DashPrefix+"js/chart.js?v="+version, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("versioned asset with gate policy → %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("gate no-store was overwritten by %q", cc)
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("gate-policy asset lost its revalidation tag")
	}
}

func TestServeDashRepresentations(t *testing.T) {
	for _, rel := range []string{"css/dashboard.css", "vendor/uplot.js"} {
		t.Run(rel, func(t *testing.T) {
			asset := staticForTest("static/" + rel)
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
				{name: "head gzip", method: http.MethodHead, accept: "gzip", gzip: true},
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
						wantLen := len(asset.body)
						if tc.gzip && len(asset.gzip) > 0 {
							wantLen = len(asset.gzip)
						}
						if res.Body.Len() != 0 || res.Header().Get("Content-Length") != strconv.Itoa(wantLen) {
							t.Fatal("HEAD must describe the selected representation without a body")
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
		asset := newStaticAsset(body, true)
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
				req.Header.Set("If-None-Match", staticForTest("static/vendor/uplot.js").etag)
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

func TestLoadStaticVersion(t *testing.T) {
	fixtureIndex := func() []byte {
		return []byte(dashboardVersionMeta +
			`<link rel="stylesheet" href="` + DashPrefix + `css/app.css?` + dashVersionQuery + `=` + dashboardVersionMarker + `">` +
			`<script defer src="` + DashPrefix + `js/app.js?` + dashVersionQuery + `=` + dashboardVersionMarker + `"></script>` +
			`<script id="dashboard-bootstrap" type="application/json">` + dashboardBootstrapMarker + `</script>`)
	}
	fixture := func() fstest.MapFS {
		files := fstest.MapFS{
			dashboardIndexPath:   {Data: fixtureIndex()},
			dashboardFaviconPath: {Data: []byte(`<svg/>`)},
			"static/css/app.css": {Data: []byte(`body{color:white}`)},
			"static/js/app.js":   {Data: []byte(`const ready=true;`)},
		}
		for _, spec := range brandByURL {
			if files[spec.file] == nil {
				files[spec.file] = &fstest.MapFile{Data: []byte("brand")}
			}
		}
		files[dashboardSWPath] = &fstest.MapFile{Data: []byte("const CACHE='millivolt-shell-__DASHBOARD_VERSION__';")}
		return files
	}
	base, version := loadStatic(fixture())
	rebuilt, rebuiltVersion := loadStatic(fixture())
	if version != rebuiltVersion || base[dashboardIndexPath].etag != rebuilt[dashboardIndexPath].etag {
		t.Fatal("identical asset rebuild changed the version or HTML validator")
	}
	served := string(base[dashboardIndexPath].body)
	if !strings.Contains(served, `content="`+version+`"`) || strings.Contains(served, dashboardVersionMarker) {
		t.Fatal("cached index did not replace every source marker with the asset version")
	}
	if got := strings.Count(served, "?"+dashVersionQuery+"="+version); got != 2 {
		t.Fatalf("cached index stamped %d asset URLs, want 2", got)
	}
	if strings.Count(served, dashboardBootstrapMarker) != 1 {
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
			nextServed := string(index.body)
			if index.etag == base[dashboardIndexPath].etag || !strings.Contains(nextServed, `content="`+nextVersion+`"`) ||
				strings.Count(nextServed, "?"+dashVersionQuery+"="+nextVersion) != 2 {
				t.Fatal("asset change did not update the served HTML version and every asset URL")
			}
			for p, old := range base {
				if p != changed && p != dashboardIndexPath && p != dashboardSWPath && old.etag != next[p].etag {
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

func completeStatic(index, sw []byte) fstest.MapFS {
	files := fstest.MapFS{
		dashboardIndexPath:   {Data: index},
		dashboardFaviconPath: {Data: []byte(`<svg/>`)},
	}
	for _, spec := range brandByURL {
		if files[spec.file] == nil {
			files[spec.file] = &fstest.MapFile{Data: []byte("brand")}
		}
	}
	if sw == nil {
		sw = []byte("const CACHE='millivolt-shell-" + dashboardVersionMarker + "';")
	}
	files[dashboardSWPath] = &fstest.MapFile{Data: sw}
	return files
}

func TestLoadStaticFailsClosed(t *testing.T) {
	versionedRef := `<script defer src="` + DashPrefix + `js/app.js?` + dashVersionQuery + `=` + dashboardVersionMarker + `"></script>`
	validIndex := []byte(dashboardVersionMeta + versionedRef + dashboardBootstrapMarker)
	validSW := []byte("const CACHE='millivolt-shell-" + dashboardVersionMarker + "';")
	cases := []struct {
		name, want string
		fsys       fs.FS
	}{
		{"missing tree", "load dashboard assets", fstest.MapFS{}},
		{"missing index", "missing " + dashboardIndexPath, fstest.MapFS{dashboardFaviconPath: {Data: []byte(`<svg/>`)}}},
		{"missing favicon", "missing ", fstest.MapFS{dashboardIndexPath: {Data: validIndex}}},
		{"missing marker", "exactly one dashboard version meta marker", completeStatic([]byte(`<html/>`+dashboardBootstrapMarker), validSW)},
		{"duplicate marker", "exactly one dashboard version meta marker", completeStatic([]byte(dashboardVersionMeta+dashboardVersionMeta+dashboardBootstrapMarker), validSW)},
		{"missing bootstrap marker", "exactly one dashboard bootstrap marker", completeStatic([]byte(dashboardVersionMeta), validSW)},
		{"duplicate bootstrap marker", "exactly one dashboard bootstrap marker", completeStatic([]byte(dashboardVersionMeta+dashboardBootstrapMarker+dashboardBootstrapMarker), validSW)},
		{"missing SW marker", "exactly one dashboard version marker", completeStatic(validIndex, []byte("const CACHE='millivolt-shell';"))},
		{"duplicate SW marker", "exactly one dashboard version marker", completeStatic(validIndex, append(append([]byte{}, validSW...), dashboardVersionMarker...))},
		{"unreadable asset", "load dashboard assets", unreadableStaticFS{fstest.MapFS{dashboardIndexPath: {Data: validIndex}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				got := recover()
				if got == nil {
					t.Fatal("invalid embedded assets returned a partial cache")
				}
				msg := fmt.Sprint(got)
				if !strings.Contains(msg, tc.want) {
					t.Fatalf("panic %q, want substring %q", msg, tc.want)
				}
			}()
			loadStatic(tc.fsys)
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

// staticForTest resolves one precomputed asset through the lazy owner.
func staticForTest(key string) *staticAsset {
	cache, _ := staticAssets()
	return cache[key]
}

// TestStaticPregzipSkipsAlreadyCompressedFormats pins the precompression
// policy: text assets keep a gzip representation, raster icons never retain a
// second full-size copy that saves only a few percent on the wire.
func TestStaticPregzipSkipsAlreadyCompressedFormats(t *testing.T) {
	cache, _ := staticAssets()
	for _, p := range []string{"static/icon-512.png", "static/icon-192.png", "static/favicon.ico"} {
		if a := cache[p]; a == nil || len(a.gzip) != 0 {
			t.Fatalf("%s must not retain a gzip representation", p)
		}
	}
	for _, p := range []string{"static/js/core.js", "static/css/dashboard.css", "static/manifest.webmanifest"} {
		if a := cache[p]; a == nil || len(a.gzip) == 0 {
			t.Fatalf("%s must retain a gzip representation", p)
		}
	}
}
