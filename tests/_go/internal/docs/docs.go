package docs

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedManifestMatchesSources(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Schema != Schema || len(manifest.Pages) < 15 {
		t.Fatalf("manifest schema/pages = %d/%d", manifest.Schema, len(manifest.Pages))
	}
	root := filepath.Join("..", "..")
	seen := map[string]bool{}
	for _, page := range manifest.Pages {
		if seen[page.Path] {
			t.Fatalf("duplicate page path %q", page.Path)
		}
		seen[page.Path] = true
		raw, err := os.ReadFile(filepath.Join(root, page.Path))
		if err != nil {
			t.Fatalf("read %s: %v", page.Path, err)
		}
		sum := sha256.Sum256(raw)
		if page.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("%s sha256 does not match the generated index; run scripts/build_site.py --write-manifest", page.Path)
		}
		if page.Bytes != len(raw) {
			t.Fatalf("%s byte count = %d, want %d", page.Path, page.Bytes, len(raw))
		}
		for _, section := range page.Sections {
			if section.ID == "" || section.Heading == "" {
				t.Fatalf("%s has an incomplete section: %+v", page.Path, section)
			}
		}
	}
}

func TestListReadAndSearch(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	list := manifest.List()
	if len(list) != len(manifest.Pages) || list[0].Path != manifest.Pages[0].Path {
		t.Fatalf("list does not preserve navigation order")
	}
	page, section, err := manifest.Read("docs/mcp.md", "")
	if err != nil || page == nil || section != nil {
		t.Fatalf("read whole page: page=%v section=%v err=%v", page, section, err)
	}
	if !strings.HasPrefix(page.Text, "# MCP server") {
		t.Fatalf("whole-page read does not start at the H1: %.60q", page.Text)
	}
	page, section, err = manifest.Read("docs/mcp.md", "tools")
	if err != nil || page == nil || section == nil || section.ID != "tools" {
		t.Fatalf("read section: page=%v section=%v err=%v", page, section, err)
	}
	if !strings.Contains(strings.ToLower(section.Text), "docs_list") {
		t.Fatalf("section text does not include the docs tools: %.120q", section.Text)
	}
	result, err := manifest.Search("operator token", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) == 0 || result.Results[0].Score == 0 {
		t.Fatalf("search returned no ranked results: %+v", result)
	}
	if result.Total < len(result.Results) {
		t.Fatalf("total %d is below returned %d", result.Total, len(result.Results))
	}
	if _, err := manifest.Search("", "", 5); err == nil {
		t.Fatal("empty query must fail")
	}
	if _, _, err := manifest.Read("docs/not-a-page.md", ""); err == nil {
		t.Fatal("unknown path must fail")
	}
}

func TestSearchLimitIsBounded(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	result, err := manifest.Search("request", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) > 20 {
		t.Fatalf("limit 100 returned %d results", len(result.Results))
	}
	zero, err := manifest.Search("request", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(zero.Results) > 8 {
		t.Fatalf("default limit returned %d results", len(zero.Results))
	}
}
