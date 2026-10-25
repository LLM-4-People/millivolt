package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

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

func (w staticBenchmarkWriter) Header() http.Header { return w.header }

func (w staticBenchmarkWriter) WriteHeader(int) {}

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
