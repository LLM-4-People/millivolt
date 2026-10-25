package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
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
	} else {
		count := int(ico[4]) | int(ico[5])<<8
		if count != 3 {
			t.Fatalf("GET /favicon.ico has %d frames, want 16/32/48", count)
		}
		sizes := map[int]bool{}
		for i := 0; i < count; i++ {
			entry := 6 + i*16
			if entry+16 > len(ico) {
				t.Fatalf("GET /favicon.ico frame %d is truncated", i)
			}
			sizes[int(ico[entry])] = true
		}
		for _, size := range []int{16, 32, 48} {
			if !sizes[size] {
				t.Fatalf("GET /favicon.ico is missing the %dpx frame", size)
			}
		}
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
