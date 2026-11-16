package web

import (
	"bytes"
	"io/fs"
	"path"
	"slices"
	"strings"
	"testing"
)

// fontFiles are the faces dashboard.css asks for. The list is derived from the
// stylesheet below, not duplicated: this literal only pins the set so a silent
// removal of a face fails the build instead of changing the product's type.
var fontFiles = []string{
	"instrument-sans.woff2",
	"instrument-sans-italic.woff2",
	"jetbrains-mono.woff2",
}

// siteOnlyFonts are embedded for the docs site and the brand plates, which
// request them from site-src/assets/site.css; the dashboard never renders the
// display face. tests/python/site.py owns their reference check.
var siteOnlyFonts = []string{"instrument-serif.woff2"}

func TestDashboardFontsAreSelfHosted(t *testing.T) {
	css, err := staticFS.ReadFile("static/css/dashboard.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range fontFiles {
		reference := []byte(`url("../fonts/` + name + `")`)
		if !bytes.Contains(css, reference) {
			t.Errorf("dashboard.css does not request %s", name)
		}
		data, err := staticFS.ReadFile("static/fonts/" + name)
		if err != nil {
			t.Errorf("missing embedded font %s: %v", name, err)
			continue
		}
		if len(data) < 4000 {
			t.Errorf("font %s is only %d bytes; a truncated face would fail silently", name, len(data))
		}
		if string(data[:4]) != "wOF2" {
			t.Errorf("font %s is not a WOFF2 file", name)
		}
	}
	// Every embedded face is requested: an orphan file would ship in the
	// binary and the docs site with no consumer.
	err = fs.WalkDir(staticFS, "static/fonts", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := path.Base(p)
		if !strings.HasSuffix(name, ".woff2") {
			return nil
		}
		if slices.Contains(siteOnlyFonts, name) {
			return nil
		}
		if !bytes.Contains(css, []byte(name)) {
			t.Errorf("embedded font %s is not requested by dashboard.css", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFontRequestsUseTheAllowlistedType(t *testing.T) {
	for _, name := range append(slices.Clone(fontFiles), siteOnlyFonts...) {
		rel, contentType, ok := dashFile(DashPrefix + "fonts/" + name)
		if !ok {
			t.Errorf("%s is not served under %s", name, DashPrefix)
			continue
		}
		if contentType != "font/woff2" {
			t.Errorf("%s is served as %s", name, contentType)
		}
		if !dashExists(rel) {
			t.Errorf("%s is allowlisted but missing from the embed", rel)
		}
		data, err := staticFS.ReadFile("static/" + rel)
		if err != nil {
			t.Fatal(err)
		}
		if asset := newStaticAsset(data, compressibleAsset(rel)); asset.gzip != nil {
			t.Errorf("%s is precompressed; WOFF2 is already compressed", name)
		}
	}
}
