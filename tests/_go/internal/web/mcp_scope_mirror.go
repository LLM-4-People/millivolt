package web

import (
	"net/url"
	"slices"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

// TestMCPScopeMirrorsTheWebFilterGrammar pins the filter grammar the MCP
// server pre-validates to the parser that owns it. The scope arguments are
// duplicated in internal/mcp so a malformed filter is a readable tool message
// instead of a bare 400, and the dimension set, the repeated-filter cap and
// the s= selector's status band drifted without a guard.
func TestMCPScopeMirrorsTheWebFilterGrammar(t *testing.T) {
	// Dimension set: exact, in both directions.
	webDims := make([]string, 0, len(validDims))
	for dim := range validDims {
		webDims = append(webDims, dim)
	}
	slices.Sort(webDims)
	mcpDims := slices.Clone(mcp.Dimensions)
	slices.Sort(mcpDims)
	if !slices.Equal(webDims, mcpDims) {
		t.Fatalf("the web parser accepts dimensions %v; mcp.Dimensions lists %v", webDims, mcpDims)
	}

	// Repeated-filter cap: find the largest count the parser accepts, then the
	// MCP pre-check must accept it and refuse one more.
	webAcceptsCount := func(count int) bool {
		values := url.Values{}
		for index := 0; index < count; index++ {
			values.Add("f", "client:x")
		}
		_, ok := parseScopeFilters(values)
		return ok
	}
	webLimit := 0
	for webLimit < 1024 && webAcceptsCount(webLimit+1) {
		webLimit++
	}
	if webLimit == 0 || webLimit == 1024 {
		t.Fatalf("the web filter cap is %d, outside the band this guard can check", webLimit)
	}
	filters := func(count int) []string {
		out := make([]string, count)
		for index := range out {
			out[index] = "client:x"
		}
		return out
	}
	if err := (mcp.Scope{Filters: filters(webLimit)}).Validate(); err != nil {
		t.Fatalf("the web parser accepts %d filters but the MCP pre-check refuses them: %v", webLimit, err)
	}
	if err := (mcp.Scope{Filters: filters(webLimit + 1)}).Validate(); err == nil {
		t.Fatalf("the web parser refuses %d filters but the MCP pre-check accepts them", webLimit+1)
	}

	// s= selector: the boundary statuses must get the same verdict from both.
	for _, status := range []string{"0", "1", "200", "9999", "10000", "-1", "abc", "99999"} {
		_, _, webOK := parseScope(url.Values{"s": []string{status}})
		mcpOK := (mcp.Scope{Status: status}).Validate() == nil
		if webOK != mcpOK {
			t.Fatalf("status %q: the web parser accepts=%v, the MCP pre-check accepts=%v", status, webOK, mcpOK)
		}
	}
}

// TestMCPScopeLiveStatusClassesMirrorTheWebSelector is the exact set pin for
// the named in-flight pills: the web owner and the MCP reference list must
// name the same classes, or a model is told a pill exists that the proxy
// refuses (or is never told about one it accepts).
func TestMCPScopeLiveStatusClassesMirrorTheWebSelector(t *testing.T) {
	webClasses := slices.Clone(liveStatusClasses)
	slices.Sort(webClasses)
	mcpClasses := slices.Clone(mcp.LiveStatusClasses)
	slices.Sort(mcpClasses)
	if !slices.Equal(webClasses, mcpClasses) {
		t.Fatalf("the web selector accepts %v; mcp.LiveStatusClasses lists %v", webClasses, mcpClasses)
	}
	for _, class := range mcp.LiveStatusClasses {
		if !liveStatusFilter(class) {
			t.Fatalf("the web selector refuses the MCP live class %q", class)
		}
	}
}
