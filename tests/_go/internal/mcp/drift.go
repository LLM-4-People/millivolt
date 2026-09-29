package mcp

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/web"
)

// TestDescribeMirrorsTheProxyOwners pins every fact this server duplicates from
// an owner it cannot import (an unexported constant) or deliberately mirrors,
// so a change on the owning side fails here instead of leaving a
// plausible-but-wrong reference in describe. Each assertion names its owner.
func TestDescribeMirrorsTheProxyOwners(t *testing.T) {
	// Status classes: internal/web statusClass returns exactly these five,
	// with cancel for 499 and err for everything else (3xx included). The
	// exact list is also pinned in TestDescribeVocabulariesAreTheProxyOnes.
	if got, want := StatusClasses, []string{"2xx", "cancel", "4xx", "5xx", "err"}; !reflect.DeepEqual(got, want) {
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
	// configured page band. describe derives its constants from that owner;
	// these exact numbers pin the band so a widened config band is a
	// deliberate edit here too.
	if logPageMin != 10 || logPageMax != 500 {
		t.Fatalf("log page band = %d..%d, want 10..500 (owner: internal/config DashLogRowsMin/Max)", logPageMin, logPageMax)
	}

	// Explorer group cap: internal/web xpNodeCap.
	if explorerMaxGroups != 24 {
		t.Fatalf("explorer group cap = %d, want 24 (owner: internal/web xpNodeCap)", explorerMaxGroups)
	}
	// Chart bucket cap: the proxy's payload maximum is internal/web
	// chartMaxBuckets+1 (the clock-aligned start can add one partial bucket).
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
	for _, tool := range listed.Tools {
		switch tool.Name {
		case "records", "audit_captures_list":
			if !strings.Contains(tool.Description, "before_ms/before_id cursor") {
				t.Fatalf("the %s description must name the cursor arguments it accepts: %q", tool.Name, tool.Description)
			}
			if strings.Contains(tool.Description, "next_before_ms/next_before_id cursor") {
				t.Fatalf("the %s description names the output fields as the cursor to send: %q", tool.Name, tool.Description)
			}
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
