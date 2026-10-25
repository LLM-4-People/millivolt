package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

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
		`name="theme-color" content="#100e0b"`,
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
	// (#100e0b = --rail, #141210 = --bg, #e8e4de = --text); these pins
	// catch a palette drift that a stylesheet can no longer equalize.
	for _, needle := range []string{
		`<meta name="theme-color" content="#100e0b">`,
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
	if manifest["theme_color"] != "#100e0b" || manifest["background_color"] != "#141210" {
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
