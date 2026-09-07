// Package web serves the embedded metrics dashboard.
package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
)

//go:embed static
var staticFS embed.FS

// DashPrefix is the URL prefix for dashboard CSS/JS. Single owner: the
// proxy catch-all routes on this, and ServeDash maps DashPrefix+rel →
// static/rel. A miss is 404, never an LLM forward. Internal constant
// (not a proxy.yaml tunable).
const DashPrefix = "/dash/"

// staticAllow is the RFC 9110 Allow value for dashboard static (index,
// favicon, DashPrefix). Mux patterns stay unmethoded so POST cannot fall
// through to the LLM proxy; the method gate is this helper.
const staticAllow = "GET, HEAD"

// dashboardVersionMarker is replaced once at startup, after the source
// assets have been fingerprinted, so the version never hashes itself.
const dashboardVersionMarker = "__DASHBOARD_VERSION__"

// dashboardBootstrapMarker is the sole insertion point for request-time
// state. It remains in the static source identity; live data never changes
// the frontend content version.
const dashboardBootstrapMarker = "__DASHBOARD_BOOTSTRAP__"

const (
	dashboardIndexPath    = "static/index.html"
	dashboardFaviconPath  = "static/favicon.svg"
	dashboardFaviconIco   = "static/favicon.ico"
	dashboardManifestPath = "static/manifest.webmanifest"
	dashboardSWPath       = "static/sw.js"
)

// brandByURL is the ungated origin-root brand/PWA surface: icons, manifest
// and service worker. Exact mux paths so they never reach the LLM catch-all.
var brandByURL = map[string]struct {
	file, ctype string
}{
	"/favicon.ico":           {dashboardFaviconIco, "image/x-icon"},
	"/favicon.svg":           {dashboardFaviconPath, "image/svg+xml"},
	"/apple-touch-icon.png":  {"static/apple-touch-icon.png", "image/png"},
	"/icon-192.png":          {"static/icon-192.png", "image/png"},
	"/icon-512.png":          {"static/icon-512.png", "image/png"},
	"/icon-192-maskable.png": {"static/icon-192-maskable.png", "image/png"},
	"/icon-512-maskable.png": {"static/icon-512-maskable.png", "image/png"},
	"/manifest.webmanifest":  {dashboardManifestPath, "application/manifest+json"},
	"/sw.js":                 {dashboardSWPath, "text/javascript; charset=utf-8"},
}

// dashTypes is the allowlisted extension → Content-Type map. Anything else
// under DashPrefix is rejected (deny by default). FileServer is not used:
// it serves POST and directory listings (net/http historical behavior).
var dashTypes = map[string]string{
	".css": "text/css; charset=utf-8",
	".js":  "text/javascript; charset=utf-8",
}

// staticAsset is a precomputed immutable asset (embeds are immutable per
// process - a rebuild changes the bytes and the tag). Source identity and
// gzip bytes are computed once at init, never on a dashboard request.
type staticAsset struct {
	etag string
	body []byte // original source is the sole ETag/version input
	gzip []byte // retained only when smaller than the original source
}

// staticCache and dashboardVersion describe the same immutable embedded
// assets. Identical rebuilds retain the version; any asset change updates it.
var staticCache, dashboardVersion = loadStatic(staticFS)

func newStaticAsset(body []byte) *staticAsset {
	a := &staticAsset{etag: `W/"` + etagHex(body) + `"`, body: body}
	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	if _, err := writer.Write(body); err != nil {
		panic(fmt.Errorf("compress dashboard asset: %w", err))
	}
	if err := writer.Close(); err != nil {
		panic(fmt.Errorf("compress dashboard asset: %w", err))
	}
	if encoded.Len() < len(body) {
		a.gzip = encoded.Bytes()
	}
	return a
}

func loadStatic(fsys fs.FS) (map[string]*staticAsset, string) {
	cache := make(map[string]*staticAsset)
	var identity strings.Builder
	// WalkDir visits filenames in lexical order. Include paths and content
	// identities with separators so additions, removals, and renames count.
	err := fs.WalkDir(fsys, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		asset := newStaticAsset(body)
		cache[p] = asset
		fmt.Fprintf(&identity, "%s\x00%s\x00", p, asset.etag)
		return nil
	})
	if err != nil {
		panic(fmt.Errorf("load dashboard assets: %w", err))
	}
	for _, p := range []string{dashboardIndexPath, dashboardFaviconPath, dashboardFaviconIco, dashboardManifestPath, dashboardSWPath} {
		if cache[p] == nil {
			panic("load dashboard assets: missing " + p)
		}
	}
	for _, spec := range brandByURL {
		if cache[spec.file] == nil {
			panic("load dashboard assets: missing " + spec.file)
		}
	}
	index := string(cache[dashboardIndexPath].body)
	if strings.Count(index, dashboardVersionMarker) != 1 {
		panic("load dashboard assets: index must contain exactly one dashboard version marker")
	}
	if strings.Count(index, dashboardBootstrapMarker) != 1 {
		panic("load dashboard assets: index must contain exactly one dashboard bootstrap marker")
	}
	sw := string(cache[dashboardSWPath].body)
	if strings.Count(sw, dashboardVersionMarker) != 1 {
		panic("load dashboard assets: service worker must contain exactly one dashboard version marker")
	}
	version := etagHex([]byte(identity.String()))
	cache[dashboardIndexPath] = newStaticAsset([]byte(strings.Replace(index, dashboardVersionMarker, version, 1)))
	cache[dashboardSWPath] = newStaticAsset([]byte(strings.Replace(sw, dashboardVersionMarker, version, 1)))
	return cache, version
}

func rejectUnlessGetHead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", staticAllow)
	http.Error(w, staticAllow+" only", http.StatusMethodNotAllowed)
	return false
}

// aggAllow is RFC 9110 Allow for dashboard JSON aggregates. These
// handlers are GET-only (no HEAD body); do not reuse rejectUnlessGetHead.
const aggAllow = "GET"

func rejectUnlessGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet {
		return true
	}
	w.Header().Set("Allow", aggAllow)
	http.Error(w, `{"error":"GET only"}`, http.StatusMethodNotAllowed)
	return false
}

// writeStaticCached serves a precomputed static asset directly - zero
// per-request sha256, ReadFile, or compression. Shared by every static
// handler; dynamic HTML/JSON instead use Gzip's live writer with the same
// acceptsGzip negotiation gate. Do not wrap static routes in Gzip again.
func writeStaticCached(w http.ResponseWriter, r *http.Request, contentType, cacheControl string, a *staticAsset) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("ETag", a.etag)
	w.Header().Add("Vary", "Accept-Encoding")
	if etagMatches(r.Header.Get("If-None-Match"), a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.Itoa(len(a.body)))
		return
	}
	body := a.body
	if len(a.gzip) > 0 && acceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		body = a.gzip
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}

// etagHex is the short sha256 identity of a static body (embeds are
// immutable per process; a rebuild changes the bytes and the tag).
func etagHex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:8])
}

// etagMatches evaluates an If-None-Match header against our ETag (RFC 9110
// §13.1.2: "*" matches any; weak comparison; comma-separated list).
func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	want := strings.TrimPrefix(etag, `W/`)
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" {
			return true
		}
		if strings.TrimPrefix(part, `W/`) == want {
			return true
		}
	}
	return false
}

// Handler serves the cached dashboard shell with the canonical bootstrap
// payload already present, avoiding a post-script round trip before first
// paint. The shell's static asset version stays immutable, but the response
// contains live records and must never use that static version as an ETag.
// The mux mounts it on / and /index.html; GET/HEAD only (deny by default).
func Handler(agg *AggAPI) http.Handler {
	if agg == nil {
		panic("dashboard requires bootstrap state")
	}
	prefix, suffix, _ := bytes.Cut(staticCache[dashboardIndexPath].body, []byte(dashboardBootstrapMarker))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rejectUnlessGetHead(w, r) {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Add("Vary", "Accept-Encoding")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			return // no body, and no snapshot/provider work for a HEAD probe
		}
		// Marshal's default HTML escaping is mandatory: records can contain
		// </script>, HTML comments, and other untrusted provider/client text.
		// An inert application/json script is parsed by the existing JS boot.
		payload, err := json.Marshal(agg.bootstrap(agg.buf.SnapshotSince(0)))
		if err != nil {
			http.Error(w, "dashboard state unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(prefix)+len(payload)+len(suffix)))
		w.Write(prefix)
		w.Write(payload)
		w.Write(suffix)
	})
}

// BrandPaths is the origin-root PWA/brand surface registered on the mux so
// those URLs never fall through to inference.
func BrandPaths() []string {
	out := make([]string, 0, len(brandByURL))
	for p := range brandByURL {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// Brand serves one ungated origin-root brand/PWA asset. Browsers fetch these
// without Authorization; they carry no dashboard data. no-cache plus ETag
// still forces revalidation after a rebuild.
func Brand(urlPath string) http.Handler {
	spec, ok := brandByURL[urlPath]
	if !ok {
		panic("unknown brand path " + urlPath)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rejectUnlessGetHead(w, r) {
			return
		}
		if urlPath == "/sw.js" {
			w.Header().Set("Service-Worker-Allowed", "/")
		}
		writeStaticCached(w, r, spec.ctype, "no-cache", staticCache[spec.file])
	})
}

// Favicon is Brand("/favicon.ico"): a real ICO so automatic /favicon.ico
// fetches (and reverse proxies that sniff image types) are not given SVG.
func Favicon() http.Handler { return Brand("/favicon.ico") }

// ServeDash is the single choke point for dashboard CSS/JS. It serves only
// allowlisted extensions from the precomputed cache (avoiding per-request
// embed.FS.ReadFile + sha256 + gzip pool cycle). Unknown paths and `..` are
// 404; other methods are 405. Never a directory listing and never a
// fall-through to the proxy.
func ServeDash(w http.ResponseWriter, r *http.Request) {
	if !rejectUnlessGetHead(w, r) {
		return
	}
	rel, ct, ok := dashFile(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a := staticCache["static/"+rel]
	if a == nil {
		http.NotFound(w, r)
		return
	}
	writeStaticCached(w, r, ct, "no-cache", a)
}

// dashFile maps a request path to an embedded relative file. ok is false
// unless the path is a clean DashPrefix + *.css or *.js under static/.
func dashFile(urlPath string) (rel, contentType string, ok bool) {
	if !strings.HasPrefix(urlPath, DashPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(urlPath, DashPrefix)
	if rest == "" {
		return "", "", false
	}
	clean := path.Clean("/" + rest) // leading slash so Clean cannot walk out
	if strings.Contains(clean, "..") || !strings.HasPrefix(clean, "/") {
		return "", "", false
	}
	rel = strings.TrimPrefix(clean, "/")
	if rel == "" || rel != path.Clean(rel) {
		return "", "", false
	}
	ct, ok := dashTypes[path.Ext(rel)]
	if !ok {
		return "", "", false
	}
	return rel, ct, true
}

// dashExists reports whether the allowlisted path is present in the embed
// (used by tests so a typo in index.html cannot 404 at runtime unnoticed).
func dashExists(rel string) bool {
	_, err := fs.Stat(staticFS, "static/"+rel)
	return err == nil
}
