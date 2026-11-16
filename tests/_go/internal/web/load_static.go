package web

import (
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

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
		files[dashboardSWPath] = &fstest.MapFile{Data: []byte("const CACHE='millivolt-shell-__DASHBOARD_VERSION__';\nconst LIVE = __LIVE_PATHS__;")}
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
		sw = []byte("const CACHE='millivolt-shell-" + dashboardVersionMarker + "';\nconst LIVE = " + dashboardSWLiveMarker + ";")
	}
	files[dashboardSWPath] = &fstest.MapFile{Data: sw}
	return files
}

func TestLoadStaticFailsClosed(t *testing.T) {
	versionedRef := `<script defer src="` + DashPrefix + `js/app.js?` + dashVersionQuery + `=` + dashboardVersionMarker + `"></script>`
	validIndex := []byte(dashboardVersionMeta + versionedRef + dashboardBootstrapMarker)
	validSW := []byte("const CACHE='millivolt-shell-" + dashboardVersionMarker + "';\nconst LIVE = " + dashboardSWLiveMarker + ";")
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
		{"missing SW marker", "exactly one dashboard version marker", completeStatic(validIndex, []byte("const CACHE='millivolt-shell';\nconst LIVE = "+dashboardSWLiveMarker+";"))},
		{"duplicate SW marker", "exactly one dashboard version marker", completeStatic(validIndex, append(append([]byte{}, validSW...), dashboardVersionMarker...))},
		{"missing SW live marker", "exactly one live-path marker", completeStatic(validIndex, []byte("const CACHE='millivolt-shell-"+dashboardVersionMarker+"';"))},
		{"duplicate SW live marker", "exactly one live-path marker", completeStatic(validIndex, append(append([]byte{}, validSW...), dashboardSWLiveMarker...))},
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

// TestNamespaceTableOwnsAllThreePolicies pins S9: the gate, the mux
// reservations and the service-worker live list all read this one table, so a
// namespace cannot be gated but cached, or reserved but forwarded. The rows are
// checked as invariants rather than a literal list so a new namespace only has
// to declare the policies that apply to it.
func TestNamespaceTableOwnsAllThreePolicies(t *testing.T) {
	gatedRoots := map[string]bool{}
	seen := map[string]bool{}
	for _, ns := range namespaces {
		if seen[ns.Root] {
			t.Errorf("namespace %s appears twice", ns.Root)
		}
		seen[ns.Root] = true
		if !strings.HasPrefix(ns.Root, "/") {
			t.Errorf("namespace %s is not rooted", ns.Root)
		}
		if ns.Gated {
			gatedRoots[ns.Root] = true
			subtree := ns.Root == "/" || Gated(ns.Root+"/x")
			if !Gated(ns.Root) || !subtree {
				t.Errorf("gated namespace %s is not gated by the gate", ns.Root)
			}
		}
	}
	// Every table root is exactly one namespace, never a prefix of another
	// (except "/", which is matched exactly).
	for _, ns := range namespaces {
		if ns.Root == "/" {
			continue
		}
		if Gated(ns.Root) != gatedRoots[ns.Root] {
			t.Errorf("gate disagrees with the table for %s", ns.Root)
		}
		if Gated(ns.Root+"/deep/path") != ns.Gated {
			t.Errorf("gate disagrees about the subtree of %s", ns.Root)
		}
	}
	// A reserved namespace must be generated into the worker list or be safely
	// cacheable; /dash is the one cacheable gated root.
	live := LiveWorkerPaths()
	for _, ns := range namespaces {
		if ns.Reserved && !slices.Contains(live, ns.Root) {
			t.Errorf("reserved namespace %s is missing from the worker live list", ns.Root)
		}
	}
	// The inference base stays ungated: provider credentials ride it untouched.
	if Gated("/v1/chat/completions") {
		t.Error("/v1 must stay ungated")
	}
	// The origin root matches only itself, not the whole origin.
	if Gated("/v1") || !Gated("/") {
		t.Errorf("root gating is wrong: /v1 gated=%v / gated=%v", Gated("/v1"), Gated("/"))
	}
	if !Gated(`/metrics\prometheus`) {
		t.Error("a decoded backslash separator is not read as one")
	}
}
