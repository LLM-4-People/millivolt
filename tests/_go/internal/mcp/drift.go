package mcp

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/web"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestDescribeMirrorsTheProxyOwners pins every fact this server duplicates from
// an owner it cannot import (an unexported constant) or deliberately mirrors.
// An importable owner is compared directly, so a change on the owning side
// fails here. An unexported cap is pinned by a literal here and by an
// owner-side literal in tests/_go/internal/web/published_limits.go, so a change
// on either side fails one of the two. Each assertion names its owner.
func TestDescribeMirrorsTheProxyOwners(t *testing.T) {
	// Status classes: internal/web statusClass returns exactly these five,
	// with cancel for 499 and err for everything else (3xx included). The
	// expected list is read from the web owner, not repeated here.
	if got, want := StatusClasses, web.StatusClasses; !reflect.DeepEqual(got, want) {
		t.Fatalf("status classes = %v, want %v (owner: internal/web statusClass)", got, want)
	}

	// Time buckets: derive the four names from metrics.TimeBucket, the
	// server-local daypart owner, instead of comparing the list to itself.
	monday := time.Date(2026, 3, 2, 0, 0, 0, 0, time.Local)
	produced := map[string]bool{}
	for _, at := range []time.Time{
		monday.Add(7 * time.Hour),                   // before 08:00
		monday.Add(9 * time.Hour),                   // 08:00 to 16:00
		monday.Add(17 * time.Hour),                  // from 16:00
		monday.AddDate(0, 0, 5).Add(10 * time.Hour), // Saturday
	} {
		produced[metrics.TimeBucket(at)] = true
	}
	if len(produced) != len(TimeBuckets) {
		t.Fatalf("metrics.TimeBucket produces %v, describe lists %v", produced, TimeBuckets)
	}
	for _, bucket := range TimeBuckets {
		if !produced[bucket] {
			t.Fatalf("describe lists the time bucket %q, which metrics.TimeBucket never returns", bucket)
		}
	}

	// Error filter keys: the join order of internal/web errorKey.
	if got, want := ErrorFilterKeys, []string{"type", "code", "message"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("error filter keys = %v, want %v (owner: internal/web errorKey)", got, want)
	}

	// Log page band: internal/config DashLogRowsMin/Max, the dashboard's own
	// configured page band. describe derives its constants from that owner, so
	// the check reads the owner too: a mirror that stops deriving (a hardcoded
	// literal that no longer tracks the config) fails here, while a deliberate
	// config band change flows through both sides. The prose pin just below
	// still forces the hand-written schema text to follow the owner.
	if logPageMin != config.DashLogRowsMin || logPageMax != config.DashLogRowsMax {
		t.Fatalf("log page band = %d..%d, want %d..%d (owner: internal/config DashLogRowsMin/Max)",
			logPageMin, logPageMax, config.DashLogRowsMin, config.DashLogRowsMax)
	}

	// The records limit argument states the same band in prose. Deriving the
	// expected phrase from the config owner and comparing the parsed band
	// exactly means a widened or tidied tag cannot keep the old numbers.
	limitField, ok := reflect.TypeOf(RecordsInput{}).FieldByName("Limit")
	if !ok {
		t.Fatal("RecordsInput has no Limit field")
	}
	wantLimit := fmt.Sprintf("page size, %d to %d", config.DashLogRowsMin, config.DashLogRowsMax)
	limitTag := string(limitField.Tag.Get("jsonschema"))
	if got := regexp.MustCompile(`page size, [0-9]+ to [0-9]+`).FindString(limitTag); got != wantLimit {
		t.Fatalf("RecordsInput.Limit schema states %q, want %q (owner: internal/config DashLogRowsMin/Max): %q",
			got, wantLimit, limitTag)
	}

	// Explorer group cap: internal/web xpNodeCap.
	if explorerMaxGroups != 24 {
		t.Fatalf("explorer group cap = %d, want 24 (owner: internal/web xpNodeCap)", explorerMaxGroups)
	}
	// Chart bucket cap: the proxy's payload maximum is internal/web
	// chartMaxBuckets+1 (the clock-aligned start can add one partial bucket).
	// The owner itself is pinned by TestChartBucketCapMatchesThePublishedReference
	// in tests/_go/internal/web/published_limits.go, so this literal catches a
	// mirror edit and that test catches an owner edit.
	if chartMaxBuckets != 31 {
		t.Fatalf("chart bucket cap = %d, want 31 (owner: internal/web chartMaxBuckets+1)", chartMaxBuckets)
	}

	// Operator token band: cmd/proxy operatorTokenMinLen/MaxLen.
	if operatorTokenMinLen != 16 || operatorTokenMaxLen != 512 {
		t.Fatalf("token band = %d..%d, want 16..512 (owner: cmd/proxy operatorTokenMinLen/MaxLen)",
			operatorTokenMinLen, operatorTokenMaxLen)
	}
}

// TestPagingToolsNameTheirRealCursorArguments pins both paging tools to the
// argument names the model actually sends. The records description told the
// model to page with next_before_ms/next_before_id, which are the OUTPUT
// fields; a model that followed it named arguments the strict input decode
// refuses. The output schema's own doc had the same confusion ("pass both next
// cursor fields"), which names no argument at all.
func TestPagingToolsNameTheirRealCursorArguments(t *testing.T) {
	session := connect(t, newTestService(t, newFakeProxy(t), Limits{}))
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range listed.Tools {
		switch tool.Name {
		case "records", "audit_captures_list":
			seen[tool.Name] = true
			if !strings.Contains(tool.Description, "before_ms/before_id cursor") {
				t.Fatalf("the %s description must name the cursor arguments it accepts: %q", tool.Name, tool.Description)
			}
			if strings.Contains(tool.Description, "next_before_ms/next_before_id cursor") {
				t.Fatalf("the %s description names the output fields as the cursor to send: %q", tool.Name, tool.Description)
			}
		}
	}
	for _, name := range []string{"records", "audit_captures_list"} {
		if !seen[name] {
			t.Fatalf("the %s tool was not inspected; removing it from the loop must not skip the cursor-name check", name)
		}
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(RecordsOutput{}), reflect.TypeOf(AuditCapturesListOutput{})} {
		for _, name := range []string{"NextBeforeMs", "NextBeforeID"} {
			field, ok := typ.FieldByName(name)
			if !ok {
				t.Fatalf("%s has no %s field", typ, name)
			}
			if tag := string(field.Tag.Get("jsonschema")); !strings.Contains(tag, "into before_ms and before_id") {
				t.Fatalf("%s.%s must name the arguments the pair is copied into, got %q", typ, name, tag)
			}
		}
	}

	// The continuation advice each listing emits must name the pair the tool
	// RETURNS. audit_captures_list used to tell the model to page with "the
	// before_ms and before_id pair this tool returned": those are the
	// arguments, and no output field is ever named that.
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	rows := make([]string, 0, 12)
	for i := range 12 {
		rows = append(rows, `{"id":"r`+strconv.Itoa(i)+`","started_at":`+strconv.Itoa(100-i)+`}`)
	}
	proxy.json(http.MethodGet, logPath,
		`{"records":[`+strings.Join(rows, ",")+`],"more":true,"cursor_ms":1,"cursor_id":"rl"}`)
	proxy.json(http.MethodGet, schemaPath, `[`+strings.Join(rows, ",")+`]`)
	limits := DefaultLimits()
	limits.PageSize = 10
	service := newTestService(t, proxy, limits)
	page, err := service.records(t.Context(), RecordsInput{})
	if err != nil {
		t.Fatal(err)
	}
	captures, err := service.auditCapturesList(t.Context(), AuditCapturesListInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tool   string
		marker string
	}{
		{"records", page.Truncation.Marker},
		{"audit_captures_list", captures.Truncation.Marker},
	} {
		if !strings.Contains(tc.marker, "next_before_ms") || !strings.Contains(tc.marker, "next_before_id") {
			t.Fatalf("%s advice %q must name the returned next_before_ms/next_before_id pair", tc.tool, tc.marker)
		}
		if strings.Contains(tc.marker, "before_ms and before_id pair this tool returned") {
			t.Fatalf("%s advice %q names the cursor arguments as output fields", tc.tool, tc.marker)
		}
	}
}

// TestSnapshotCapNamesTheRingOwner pins the "8 * dash_log_rows" formula to
// web.BootRingCapMul, the exported owner of the snapshot multiplier. The
// production text states the multiplier as a number because the
// per-deployment dash_log_rows value is not known here, and importing
// internal/web would embed the whole dashboard in the MCP binary.
func TestSnapshotCapNamesTheRingOwner(t *testing.T) {
	formula := strconv.Itoa(web.BootRingCapMul) + " * dash_log_rows"
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	proxy.json(http.MethodGet, debugPath, emptyDebugStatus)
	proxy.json(http.MethodGet, schemaPath, `[]`)
	reference, err := newTestService(t, proxy, Limits{}).describe(t.Context(), DescribeInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(reference.Notes, " "), formula) {
		t.Fatalf("the describe note must state the snapshot cap %q: %q", formula, reference.Notes)
	}
	session := connect(t, newTestService(t, newFakeProxy(t), Limits{}))
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name != "snapshot" {
			continue
		}
		if !strings.Contains(tool.Description, formula) {
			t.Fatalf("the snapshot description must state %q: %q", formula, tool.Description)
		}
		return
	}
	t.Fatal("the snapshot tool is not registered")
}

// TestPurgePhraseHasOneOwner pins the confirmation phrase to PurgeConfirmation:
// the input schema and the tool description must carry it, and the published
// guide must quote the same literal.
func TestPurgePhraseHasOneOwner(t *testing.T) {
	field, ok := reflect.TypeOf(PurgeInput{}).FieldByName("Confirmation")
	if !ok {
		t.Fatal("PurgeInput has no Confirmation field")
	}
	tag := string(field.Tag.Get("jsonschema"))
	if !strings.Contains(tag, PurgeConfirmation) {
		t.Fatalf("the confirmation schema tag must carry the owner phrase, got %q", tag)
	}
	path := filepath.Join("..", "..", "docs", "mcp.md")
	guide, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the MCP guide: %v", err)
	}
	if !strings.Contains(string(guide), PurgeConfirmation) {
		t.Fatalf("docs/mcp.md must quote the exact confirmation phrase %q", PurgeConfirmation)
	}
}

// TestProtocolVersionsMirrorTheSDK pins the accepted protocol versions listed
// in docs/mcp.md to the bundled SDK's own list, which is the owner of that set.
// The guide's hand list used to be unguarded, so a dependency upgrade or a doc
// edit could silently change what the endpoint accepts or claims to accept.
func TestProtocolVersionsMirrorTheSDK(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "mcp.md"))
	if err != nil {
		t.Fatalf("read the MCP guide: %v", err)
	}
	const heading = "The accepted protocol versions are the bundled SDK's full set, newest first:"
	index := strings.Index(string(guide), heading)
	if index < 0 {
		t.Fatalf("docs/mcp.md must state %q", heading)
	}
	// The list is the sentence right after the heading; the next sentence
	// repeats some versions in prose, so the scan stops at the first period.
	sentence := string(guide)[index+len(heading):]
	if dot := strings.Index(sentence, "."); dot >= 0 {
		sentence = sentence[:dot]
	}
	matches := regexp.MustCompile("`([0-9]{4}-[0-9]{2}-[0-9]{2})`").FindAllStringSubmatch(sentence, -1)
	listed := make([]string, 0, len(matches))
	for _, match := range matches {
		listed = append(listed, match[1])
	}
	if want := sdk.SupportedProtocolVersions(); !reflect.DeepEqual(listed, want) {
		t.Fatalf("docs/mcp.md lists %v, the SDK supports %v (owner: github.com/modelcontextprotocol/go-sdk/mcp SupportedProtocolVersions)", listed, want)
	}
}

// TestToolTableMatchesTheRegistry pins the hand-written tools table in
// docs/mcp.md to the registered surface: a renamed, missing or extra row fails
// here, so the published list cannot drift from the tools the server serves.
// The comparison is bidirectional: a registered tool with no row is as wrong
// as a row naming a tool the registry does not serve.
func TestToolTableMatchesTheRegistry(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "mcp.md"))
	if err != nil {
		t.Fatalf("read the MCP guide: %v", err)
	}
	const heading = "## Tools"
	index := strings.Index(string(guide), heading)
	if index < 0 {
		t.Fatalf("docs/mcp.md must keep the %q section that lists every tool", heading)
	}
	// A tool row starts with its backticked name in the first cell; the header
	// and separator rows do not match.
	row := regexp.MustCompile("^\\| `([a-z][a-z0-9_]*)` \\|")
	seen := map[string]int{}
	rows := 0
	started := false
	for _, line := range strings.Split(string(guide)[index+len(heading):], "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "|") {
			started = true
			if match := row.FindStringSubmatch(trimmed); match != nil {
				seen[match[1]]++
				rows++
			}
			continue
		}
		if started && trimmed != "" {
			break
		}
	}
	if rows == 0 {
		t.Fatal("docs/mcp.md's tools table has no rows naming a tool")
	}
	session := connect(t, newTestService(t, newFakeProxy(t), Limits{}))
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, count := range seen {
		if count > 1 {
			t.Fatalf("the tools table lists %s %d times", name, count)
		}
		registered := false
		for _, tool := range listed.Tools {
			if tool.Name == name {
				registered = true
				break
			}
		}
		if !registered {
			t.Fatalf("the tools table lists %s, which the registry does not serve", name)
		}
	}
	for _, tool := range listed.Tools {
		if seen[tool.Name] == 0 {
			t.Fatalf("the tools table omits %s, which the registry serves", tool.Name)
		}
	}
}

// TestDocumentedToolCountMatchesTheRegistry pins every hand-written "N tools"
// claim in the published docs to the registered tool surface, which is the
// owner: adding or removing a tool must update each mention, and a doc-only
// number edit fails here. Every mention form is guarded, including the
// hyphenated adjective in README.md, and every file that states a count must
// keep stating one. The docs keep the number rather than a count-free phrase
// because the placements read better with it; this guard is what keeps it
// true.
func TestDocumentedToolCountMatchesTheRegistry(t *testing.T) {
	session := connect(t, newTestService(t, newFakeProxy(t), Limits{}))
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := strconv.Itoa(len(listed.Tools))
	mentions := regexp.MustCompile(`([0-9]+)(?:-tools?|\s+tools?)\b`)
	for _, doc := range []struct{ label, path string }{
		{"README.md", filepath.Join("..", "..", "README.md")},
		{"docs/mcp.md", filepath.Join("..", "..", "docs", "mcp.md")},
		{"docs/operations.md", filepath.Join("..", "..", "docs", "operations.md")},
	} {
		guide, err := os.ReadFile(doc.path)
		if err != nil {
			t.Fatalf("read %s: %v", doc.path, err)
		}
		found := mentions.FindAllStringSubmatch(string(guide), -1)
		if len(found) == 0 {
			t.Fatalf("%s no longer states how many tools are served", doc.label)
		}
		for _, match := range found {
			if match[1] != want {
				t.Fatalf("%s states %q, the registry serves %s tools", doc.label, match[0], want)
			}
		}
	}
}
