package web

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

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

	// The Settings sheet is lazy: chrome_settings.js owns one ordered
	// SETTINGS_MODULES list, the trigger injects each module with the meta
	// version on first open, so the first-paint index must not carry any
	// js/settings* asset and the walk exemption is membership in that list.
	for _, ref := range rels {
		if strings.HasPrefix(ref.rel, "js/settings") {
			t.Errorf("index.html must not load %s on first paint", ref.rel)
		}
	}
	chrome, err := staticFS.ReadFile("static/js/chrome_settings.js")
	if err != nil {
		t.Fatal(err)
	}
	settingsModule := map[string]bool{}
	for _, rel := range settingsModuleList(t, chrome) {
		settingsModule[rel] = true
		if !dashExists(rel) {
			t.Errorf("SETTINGS_MODULES names %s but embedded static/%s is missing", rel, rel)
		}
	}
	// One URL builder must carry the dashboard version for every listed
	// module: an unversioned lazy URL would pin stale bytes across a rebuild.
	if !bytes.Contains(chrome, []byte(`'/dash/js/' + name + '?v='`)) ||
		!bytes.Contains(chrome, []byte(`encodeURIComponent(version)`)) {
		t.Error("chrome_settings.js does not build each settings module URL from SETTINGS_MODULES and the dashboard version")
	}
	if !bytes.Contains(chrome, []byte(`function loadSettingsModule(`)) {
		t.Error("chrome_settings.js is missing the sequential Settings loader")
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
		if settingsModule[rel] {
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

// settingsModuleList extracts the ordered SETTINGS_MODULES owner list from
// chrome_settings.js and returns its entries as embed-relative js/ paths. The list is
// the single owner of the lazy Settings scripts, so it fails loudly on a
// missing declaration, an entry outside js/settings*.js, a duplicate, an
// empty list, or an entry-first order that would load an editor first.
func settingsModuleList(t *testing.T, chrome []byte) []string {
	t.Helper()
	m := regexp.MustCompile(`(?s)const SETTINGS_MODULES = \[(.*?)\];`).FindSubmatch(chrome)
	if m == nil {
		t.Fatal("chrome_settings.js missing const SETTINGS_MODULES")
	}
	var mods []string
	seen := map[string]bool{}
	for _, q := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(m[1], -1) {
		name := string(q[1])
		if !strings.HasPrefix(name, "settings") || !strings.HasSuffix(name, ".js") {
			t.Errorf("SETTINGS_MODULES entry %q is not a settings*.js module", name)
			continue
		}
		rel := "js/" + name
		if seen[rel] {
			t.Errorf("SETTINGS_MODULES repeats %q", rel)
			continue
		}
		seen[rel] = true
		mods = append(mods, rel)
	}
	if len(mods) == 0 {
		t.Fatal("SETTINGS_MODULES lists no modules")
	}
	if mods[0] != "js/settings.js" {
		t.Errorf("SETTINGS_MODULES[0] = %q, want the js/settings.js entry", mods[0])
	}
	return mods
}
