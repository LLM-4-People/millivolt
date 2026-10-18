// Package web serves the embedded metrics dashboard.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
)

//go:embed static
var staticFS embed.FS

// DashPrefix is the URL prefix for dashboard CSS/JS. Single owner: the
// proxy catch-all routes on this, and ServeDash maps DashPrefix+rel →
// static/rel. A miss is 404, never an LLM forward. Internal constant
// (not a proxy.yaml tunable).
const DashPrefix = "/dash/"

const (
	// staticRevalidateCacheControl makes the browser revalidate a static
	// response against its ETag on every load. Unversioned or stale-version
	// /dash URLs, and every brand/PWA path, keep this policy so an old index
	// cannot pin new bytes under an old URL.
	staticRevalidateCacheControl = "no-cache"
	// dashImmutableCacheControl is safe only for a /dash URL whose v query
	// exactly names this process's embedded content version: a rebuild
	// changes the version before it can change the bytes under that URL.
	// It is private to the operator's browser: a Bearer-authenticated first
	// load can also carry a session cookie, so a shared cache must not retain
	// that response even though the asset bytes themselves are not secret.
	dashImmutableCacheControl = "private, max-age=31536000, immutable"
)

// dashVersionQuery is the local asset URL's content-version query key. The
// shell stamps it from the same marker as its meta identity; ServeDash matches
// it and never lets it participate in the asset lookup path.
const dashVersionQuery = "v"

// staticAllow is the RFC 9110 Allow value for dashboard static (index,
// favicon, DashPrefix). Mux patterns stay unmethoded so POST cannot fall
// through to the LLM proxy; the method gate is this helper.
const staticAllow = "GET, HEAD"

// dashboardVersionMarker is replaced at startup after the source assets have
// been fingerprinted, so the version never hashes itself. The shell carries it
// in the authoritative meta stamp and in every local asset URL's v query; the
// service worker carries it once in its cache name.
const dashboardVersionMarker = "__DASHBOARD_VERSION__"

// dashboardVersionMeta is the shell's single authoritative version stamp,
// required exactly once before loadStatic replaces every marker occurrence.
const dashboardVersionMeta = `<meta name="dashboard-version" content="` + dashboardVersionMarker + `">`

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

// staticAssets lazily describes the immutable embedded assets. Identical
// rebuilds retain the version; any asset change updates it. The walk, hashing
// and precompression run on the first dashboard/brand request, not in every
// short-lived CLI mode: the container healthcheck executes this binary every
// 30 seconds and serves nothing.
var (
	staticOnce       sync.Once
	staticCache      map[string]*staticAsset
	dashboardVersion string
)

func staticAssets() (map[string]*staticAsset, string) {
	staticOnce.Do(func() { staticCache, dashboardVersion = loadStatic(staticFS) })
	return staticCache, dashboardVersion
}

// dashboardContentVersion is the bootstrap payload's asset identity.
func dashboardContentVersion() string {
	_, version := staticAssets()
	return version
}

// compressibleAsset is false for formats whose bytes are already compressed:
// gzip on a PNG saves about 5% while retaining a second full-size copy and
// making clients decompress it again.
func compressibleAsset(p string) bool {
	switch path.Ext(p) {
	case ".png", ".ico":
		return false
	}
	return true
}

func newStaticAsset(body []byte, compress bool) *staticAsset {
	a := &staticAsset{etag: `W/"` + etagHex(body) + `"`, body: body}
	if !compress {
		return a
	}
	var encoded bytes.Buffer
	writer := newPrecomputedGzipWriter(&encoded)
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
		asset := newStaticAsset(body, compressibleAsset(p))
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
	if strings.Count(index, dashboardVersionMeta) != 1 {
		panic("load dashboard assets: index must contain exactly one dashboard version meta marker")
	}
	if strings.Count(index, dashboardBootstrapMarker) != 1 {
		panic("load dashboard assets: index must contain exactly one dashboard bootstrap marker")
	}
	sw := string(cache[dashboardSWPath].body)
	if strings.Count(sw, dashboardVersionMarker) != 1 {
		panic("load dashboard assets: service worker must contain exactly one dashboard version marker")
	}
	version := etagHex([]byte(identity.String()))
	// Every shell marker is intentional: the meta stamp, one per local asset
	// URL, and the service worker's cache name. Replace all of them so the
	// served index never advertises the source spelling.
	cache[dashboardIndexPath] = newStaticAsset([]byte(strings.ReplaceAll(index, dashboardVersionMarker, version)), true)
	cache[dashboardSWPath] = newStaticAsset([]byte(strings.Replace(sw, dashboardVersionMarker, version, 1)), true)
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

// writeStaticCached serves a precomputed static asset directly - zero
// per-request sha256, ReadFile, or compression. Shared by every static
// handler; dynamic HTML/JSON instead use Gzip's live writer with the same
// acceptsGzip negotiation gate. Do not wrap static routes in Gzip again.
func writeStaticCached(w http.ResponseWriter, r *http.Request, contentType, cacheControl string, a *staticAsset) {
	w.Header().Set("Content-Type", contentType)
	// The operator gate can set no-store before this handler runs, for an
	// authenticated response that also mints a session cookie. Never override
	// a stricter policy that an earlier owner already set.
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.Header().Set("ETag", a.etag)
	w.Header().Add("Vary", "Accept-Encoding")
	if etagMatches(r.Header.Get("If-None-Match"), a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := a.body
	if len(a.gzip) > 0 && acceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		body = a.gzip
	}
	// HEAD describes the representation GET would select, including the gzip
	// body's length; advertising the identity length for a gzip response makes
	// caches and CDNs size the wrong representation.
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
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
	cache, _ := staticAssets()
	prefix, suffix, _ := bytes.Cut(cache[dashboardIndexPath].body, []byte(dashboardBootstrapMarker))
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
			log.Printf("dashboard: encode bootstrap: %v", err)
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
	return slices.Sorted(maps.Keys(brandByURL))
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
		cache, _ := staticAssets()
		writeStaticCached(w, r, spec.ctype, staticRevalidateCacheControl, cache[spec.file])
	})
}

// ServeDash is the single choke point for dashboard CSS/JS. It serves only
// allowlisted extensions from the precomputed cache (avoiding per-request
// embed.FS.ReadFile + sha256 + gzip pool cycle). A URL whose v query exactly
// matches the embedded content version is immutable; a missing or stale
// version keeps no-cache revalidation, so an old index cannot pin new bytes
// under an old URL. The query never participates in the asset lookup path.
// Unknown paths and `..` are 404; other methods are 405. Never a directory
// listing and never a fall-through to the proxy.
func ServeDash(w http.ResponseWriter, r *http.Request) {
	if !rejectUnlessGetHead(w, r) {
		return
	}
	rel, ct, ok := dashFile(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	cache, version := staticAssets()
	a := cache["static/"+rel]
	if a == nil {
		http.NotFound(w, r)
		return
	}
	cacheControl := staticRevalidateCacheControl
	if values := r.URL.Query()[dashVersionQuery]; len(values) == 1 && values[0] == version {
		cacheControl = dashImmutableCacheControl
	}
	writeStaticCached(w, r, ct, cacheControl, a)
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
