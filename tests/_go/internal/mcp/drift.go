package mcp

import (
	"flag"
	"fmt"
	"io"
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
// number edit fails here. The guarded forms are the hyphenated adjective in
// README.md ("22-tool") and the separated forms with at most one qualifier
// word ("22 tools", "22 MCP tools", "22 registered tools"), and every file
// that states a count must keep stating one. The docs keep the number rather
// than a count-free phrase because the placements read better with it; this
// guard is what keeps it true.
func TestDocumentedToolCountMatchesTheRegistry(t *testing.T) {
	session := connect(t, newTestService(t, newFakeProxy(t), Limits{}))
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := strconv.Itoa(len(listed.Tools))
	// The optional qualifier word covers the separated forms ("22 MCP tools",
	// "22 registered tools") that the plain `\s+tools?` alternative missed.
	mentions := regexp.MustCompile(`([0-9]+)(?:-tools?|[ \t]+(?:[A-Za-z]+[ \t]+)?tools?)\b`)
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

// TestSetupTableMatchesTheFlags pins the setup table in docs/mcp.md to the
// programmatic owner: the flag set registerOptions installs and the defaults
// DefaultLimits() supplies (both resolved by the flag registration itself).
// A changed default, a renamed flag or a renamed environment variable fails
// here instead of shipping a guide that no longer sets up the server. The
// environment name is read from the registered usage text, which is built from
// the same constant the resolution reads, so the guide, the usage and the
// lookup cannot disagree.
func TestSetupTableMatchesTheFlags(t *testing.T) {
	var o options
	o.limits = DefaultLimits()
	fs := flag.NewFlagSet("millivolt-mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerOptions(fs, &o)

	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "mcp.md"))
	if err != nil {
		t.Fatalf("read the MCP guide: %v", err)
	}
	const heading = "## Setup"
	index := strings.Index(string(guide), heading)
	if index < 0 {
		t.Fatalf("docs/mcp.md must keep the %q section that documents the setup surface", heading)
	}
	section := string(guide)[index+len(heading):]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	// A table body row starts with a backticked flag; the header and separator
	// rows do not match. The default cell is either backticked or the literal
	// (required) the two credential flags use. The last captured cell is the
	// meaning column, which the token-source guard below reads.
	row := regexp.MustCompile("^\\| `(--[a-z0-9-]+)` \\| `([A-Z0-9_]+)` \\| (?:(`([^`]+)`)|(\\(required\\))) \\| (.*) \\|$")
	documented := map[string]string{}
	documentedEnv := map[string]string{}
	documentedMeaning := map[string]string{}
	for _, line := range strings.Split(section, "\n") {
		match := row.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		if _, duplicate := documented[match[1]]; duplicate {
			t.Fatalf("the setup table lists %s more than once", match[1])
		}
		def := match[4]
		if def == "" {
			def = match[5]
		}
		documented[match[1]] = def
		documentedEnv[match[1]] = match[2]
		documentedMeaning[match[1]] = match[6]
	}
	if len(documented) == 0 {
		t.Fatal("docs/mcp.md's setup table has no flag rows")
	}
	usageEnv := regexp.MustCompile(`\(env ([A-Z0-9_]+)\)`)
	fs.VisitAll(func(f *flag.Flag) {
		def, ok := documented["--"+f.Name]
		if !ok {
			t.Fatalf("docs/mcp.md's setup table omits --%s", f.Name)
		}
		match := usageEnv.FindStringSubmatch(f.Usage)
		if match == nil {
			t.Fatalf("the --%s usage text must name its environment variable", f.Name)
		}
		if got := documentedEnv["--"+f.Name]; got != match[1] {
			t.Fatalf("docs/mcp.md pairs --%s with %s, the owner uses %s", f.Name, got, match[1])
		}
		getter, ok := f.Value.(flag.Getter)
		if !ok {
			t.Fatalf("--%s is not a flag.Getter", f.Name)
		}
		switch value := getter.Get().(type) {
		case int:
			got, err := strconv.Atoi(def)
			if err != nil || got != value {
				t.Fatalf("docs/mcp.md states --%s default %q, the owner default is %d", f.Name, def, value)
			}
		case time.Duration:
			got, err := time.ParseDuration(def)
			if err != nil || got != value {
				t.Fatalf("docs/mcp.md states --%s default %q, the owner default is %s", f.Name, def, value)
			}
		case string:
			// The two credential flags are deliberately required: the table
			// says so and the registration leaves them empty.
			if value != "" || def != "(required)" {
				t.Fatalf("docs/mcp.md states --%s default %q, the owner default is %q", f.Name, def, value)
			}
		default:
			t.Fatalf("--%s has unexpected flag type %T", f.Name, value)
		}
	})
	// The --operator-token usage says where the VALUE comes from: the proxy's
	// own proxyTokenEnv constant, not this server's read variable. The phrase
	// once named the read variable as the proxy's, sending a user to a variable
	// the proxy never reads; both the usage text and the table row carry the
	// owner constant so the phrase cannot drift back.
	token := fs.Lookup(flagOperatorToken)
	if token == nil {
		t.Fatalf("--%s is not registered", flagOperatorToken)
	}
	if !strings.Contains(token.Usage, "the proxy's "+proxyTokenEnv) {
		t.Fatalf("the --%s usage must name the proxy's %s as the value's source, got %q",
			flagOperatorToken, proxyTokenEnv, token.Usage)
	}
	if !strings.Contains(documentedMeaning["--"+flagOperatorToken], proxyTokenEnv) {
		t.Fatalf("docs/mcp.md's --%s row must name the proxy's %s as the value's source, got %q",
			flagOperatorToken, proxyTokenEnv, documentedMeaning["--"+flagOperatorToken])
	}
	for name := range documented {
		if fs.Lookup(strings.TrimPrefix(name, "--")) == nil {
			t.Fatalf("docs/mcp.md documents %s, which the owner does not register", name)
		}
	}
}

// TestPackageSetupDocMatchesTheFlags pins cmd/mcp's package-doc setup list to
// the registered surface. A doc comment cannot interpolate the constants
// (package main cannot see them), so that list is a hand-written repeat the
// owner's guard does not reach; this compares it to the same registration
// TestSetupTableMatchesTheFlags reads, so a flag or environment rename that
// updates the code but not the package doc fails here.
func TestPackageSetupDocMatchesTheFlags(t *testing.T) {
	var o options
	o.limits = DefaultLimits()
	fs := flag.NewFlagSet("millivolt-mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerOptions(fs, &o)

	doc, err := os.ReadFile(filepath.Join("..", "..", "cmd", "mcp", "main.go"))
	if err != nil {
		t.Fatalf("read cmd/mcp/main.go: %v", err)
	}
	// A doc line is `//<tab>--flag / ENV_NAME` with alignment spaces.
	row := regexp.MustCompile(`^//\t(--[a-z0-9-]+)\s+/\s+([A-Z0-9_]+)`)
	documented := map[string]string{}
	for _, line := range strings.Split(string(doc), "\n") {
		if match := row.FindStringSubmatch(line); match != nil {
			if _, duplicate := documented[match[1]]; duplicate {
				t.Fatalf("cmd/mcp's package doc lists %s more than once", match[1])
			}
			documented[match[1]] = match[2]
		}
	}
	if len(documented) == 0 {
		t.Fatal("cmd/mcp's package doc no longer lists the setup surface")
	}
	usageEnv := regexp.MustCompile(`\(env ([A-Z0-9_]+)\)`)
	fs.VisitAll(func(f *flag.Flag) {
		name := "--" + f.Name
		env, ok := documented[name]
		if !ok {
			t.Fatalf("cmd/mcp's package doc omits %s", name)
		}
		match := usageEnv.FindStringSubmatch(f.Usage)
		if match == nil {
			t.Fatalf("the %s usage text must name its environment variable", name)
		}
		if env != match[1] {
			t.Fatalf("cmd/mcp's package doc pairs %s with %s, the owner uses %s", name, env, match[1])
		}
	})
	for name := range documented {
		if fs.Lookup(strings.TrimPrefix(name, "--")) == nil {
			t.Fatalf("cmd/mcp's package doc lists %s, which the owner does not register", name)
		}
	}
}

// TestGuidePinsTheLimitAndRedactionOwners pins the hand-written numbers in
// docs/mcp.md to the constants that own them: the token band, the explorer and
// chart caps, the credential fragment floor and the excerpt bound with its
// truncation marker. Each expected phrase is derived from the owner, so an
// owner edit fails here until the guide follows, and a guide edit fails too.
// The owners' own cross-checks live in TestDescribeMirrorsTheProxyOwners and
// the client tests; this guard covers the published prose.
func TestGuidePinsTheLimitAndRedactionOwners(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "mcp.md"))
	if err != nil {
		t.Fatalf("read the MCP guide: %v", err)
	}
	text := string(guide)

	// Operator token band: internal/mcp/limits.go operatorTokenMinLen/MaxLen.
	band := regexp.MustCompile(`band matches the proxy's own boot check \((\d+) to (\d+)\s+characters\)`).FindStringSubmatch(text)
	if band == nil {
		t.Fatal("docs/mcp.md must state the token band sentence")
	}
	if band[1] != strconv.Itoa(operatorTokenMinLen) || band[2] != strconv.Itoa(operatorTokenMaxLen) {
		t.Fatalf("docs/mcp.md states the token band %s to %s, the owner is %d to %d",
			band[1], band[2], operatorTokenMinLen, operatorTokenMaxLen)
	}

	// Explorer and chart caps: internal/mcp/describe.go explorerMaxGroups and
	// chartMaxBuckets.
	for _, phrase := range []string{
		fmt.Sprintf("with at most %d groups", explorerMaxGroups),
		fmt.Sprintf("at most %d clock-aligned buckets", chartMaxBuckets),
	} {
		if !strings.Contains(text, phrase) {
			t.Fatalf("docs/mcp.md must state %q (the tools table row cap)", phrase)
		}
	}
	caps := regexp.MustCompile(`caps groups at (\d+) and the chart caps buckets\s+at\s+(\d+)\s+SERVER-SIDE`).FindStringSubmatch(text)
	if caps == nil {
		t.Fatal("docs/mcp.md must state the server-side group and bucket caps")
	}
	if caps[1] != strconv.Itoa(explorerMaxGroups) || caps[2] != strconv.Itoa(chartMaxBuckets) {
		t.Fatalf("docs/mcp.md states server-side caps %s and %s, the owners are %d and %d",
			caps[1], caps[2], explorerMaxGroups, chartMaxBuckets)
	}

	// Credential fragment floor: internal/mcp/client.go minRedactionRun.
	if phrase := fmt.Sprintf("any contiguous %d-byte fragment", minRedactionRun); !strings.Contains(text, phrase) {
		t.Fatalf("docs/mcp.md must state %q (the credential fragment floor)", phrase)
	}

	// Excerpt bound: internal/mcp/client.go maxErrorBodyBytes and
	// truncationMarker, whose length the guide quotes as well.
	if maxErrorBodyBytes%1024 != 0 {
		t.Fatalf("maxErrorBodyBytes = %d is no longer a whole number of KiB; update the guide and this guard", maxErrorBodyBytes)
	}
	kib := maxErrorBodyBytes / 1024
	// The guide wraps "truncated to" and the size onto separate lines, so the
	// derived phrase starts at the size.
	for _, phrase := range []string{
		fmt.Sprintf("%d KiB before the `%s` marker is appended", kib, truncationMarker),
		fmt.Sprintf("runs up to the %d-byte marker past %d KiB", len(truncationMarker), kib),
	} {
		if !strings.Contains(text, phrase) {
			t.Fatalf("docs/mcp.md must state %q (the excerpt bound)", phrase)
		}
	}
}

// TestGuidePinsTheFilterVocabularies pins the three hand-written value lists in
// docs/mcp.md to their owners in internal/mcp: the filter dimensions
// (Dimensions), the time dayparts (TimeBuckets) and the status classes
// (StatusClasses). The backticked list after each heading is parsed in order
// and compared exactly, so a value added to either side fails until both match.
func TestGuidePinsTheFilterVocabularies(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "mcp.md"))
	if err != nil {
		t.Fatalf("read the MCP guide: %v", err)
	}
	text := string(guide)
	for _, tc := range []struct {
		name    string
		heading string
		want    []string
	}{
		{"filter dimensions", "`dim` is one of ", Dimensions},
		{"time dayparts", "buckets are ", TimeBuckets},
		{"status classes", "not HTTP shorthand: ", StatusClasses},
	} {
		index := strings.Index(text, tc.heading)
		if index < 0 {
			t.Fatalf("docs/mcp.md must keep the %s sentence starting %q", tc.name, tc.heading)
		}
		// The list is the sentence right after the heading; the next sentence
		// describes the values, so the scan stops at the first period.
		sentence := text[index+len(tc.heading):]
		if dot := strings.Index(sentence, "."); dot >= 0 {
			sentence = sentence[:dot]
		}
		matches := regexp.MustCompile("`([a-z0-9]+)`").FindAllStringSubmatch(sentence, -1)
		listed := make([]string, 0, len(matches))
		for _, match := range matches {
			listed = append(listed, match[1])
		}
		if !reflect.DeepEqual(listed, tc.want) {
			t.Fatalf("docs/mcp.md lists %v for the %s, the owner is %v", listed, tc.name, tc.want)
		}
	}
}
