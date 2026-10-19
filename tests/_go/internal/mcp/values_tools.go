package mcp

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// TestValuesListsADimensionVocabulary pins the discovery surface. The gap it
// fills is consequential: a misspelled client, provider, model, tool,
// conversation or key is an EMPTY result from explore, chart and records, never
// an error, so a model that guesses converts a recoverable mistake into a
// confidently wrong answer.
func TestValuesListsADimensionVocabulary(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, schemaPath,
		`[{"value":"dev-traffic","requests":900},{"value":"cli","requests":12}]`)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	out, err := service.values(ctx, ValuesInput{Dim: "client"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Dim != "client" || out.Returned != 2 || len(out.Values) != 2 {
		t.Fatalf("values output = %+v", out)
	}
	if out.Values[0].Value != "dev-traffic" || out.Values[0].Requests != 900 {
		t.Fatalf("values must be most frequent first with their counts: %+v", out.Values)
	}
	if out.Truncation.Truncated {
		t.Fatalf("two values under the cap must not claim truncation: %+v", out.Truncation)
	}

	// The statement is the closed set's own: a column-valued probe for client.
	probe := proxy.requestsFor(http.MethodGet, schemaPath)[0]
	assertAuth(t, probe)
	statement := probe.Query.Get("q")
	for _, needle := range []string{"FROM requests", "AS value", "COUNT(*)", "GROUP BY value", "LIMIT"} {
		if !strings.Contains(statement, needle) {
			t.Fatalf("statement %q must contain %q", statement, needle)
		}
	}
	if strings.Contains(statement, "value'") {
		t.Fatalf("the statement must never interpolate caller text: %q", statement)
	}

	// The note is where the model learns the traps: an unknown value is an empty
	// result, and time/error are derived rather than stored.
	for _, needle := range []string{"EMPTY result", "time and error are not listed", "status FILTER"} {
		if !strings.Contains(out.Note, needle) {
			t.Fatalf("note must mention %q, got %q", needle, out.Note)
		}
	}
}

// TestValuesToolProbeIsPinned pins one statement per probeable dimension, so a
// new dimension cannot be added without a statement and a removed column cannot
// leave a probe that returns a confusing empty result.
func TestValuesToolProbeIsPinned(t *testing.T) {
	want := map[string][]string{
		"client":        {"client AS value", "FROM requests"},
		"provider":      {"provider AS value", "FROM requests"},
		"conversation":  {"conversation_id AS value", "FROM requests"},
		"key":           {"key_hash AS value", "FROM requests"},
		"status":        {"CAST(status_code AS TEXT) AS value", "FROM requests"},
		"error_type":    {"error_type AS value", "error_type != ''"},
		"error_code":    {"error_code AS value", "error_code != ''"},
		"error_message": {"error_msg AS value", "error_msg != ''"},
		"tool":          {"json_each(r.tool_names)", "j.value != ''", "AS value"},
	}
	if len(valuesDimensions) != len(want) {
		t.Fatalf("non-model probeable dimensions = %d, want %d", len(valuesDimensions), len(want))
	}
	for dim, needles := range want {
		build, ok := valuesDimensions[dim]
		if !ok {
			t.Fatalf("dimension %q has no probe", dim)
		}
		statement := build(11)
		for _, needle := range needles {
			if !strings.Contains(statement, needle) {
				t.Fatalf("the %q probe %q must contain %q", dim, statement, needle)
			}
		}
		if !strings.HasSuffix(statement, " LIMIT 11") {
			t.Fatalf("the %q probe must bound itself: %q", dim, statement)
		}
	}
	// model has its own paged probe: ordered by raw spelling for a keyset
	// cursor, one bounded page per query, never the whole vocabulary at once.
	modelProbe := modelPageStatement(nil, 11)
	for _, needle := range []string{"model AS value", "COUNT(*)", "FROM requests", "GROUP BY value", "ORDER BY value ASC", "LIMIT 11"} {
		if !strings.Contains(modelProbe, needle) {
			t.Fatalf("the model probe %q must contain %q", modelProbe, needle)
		}
	}
	after := "glm-5-3"
	paged := modelPageStatement(&after, 11)
	if !strings.Contains(paged, "WHERE model > CAST(X'676c6d2d352d33' AS TEXT)") {
		t.Fatalf("the model page must keyset after the last spelling as a hex text expression: %q", paged)
	}
	if strings.Contains(paged, "glm-5-3") {
		t.Fatalf("the raw cursor must never enter the statement source: %q", paged)
	}
	if !strings.Contains(probeableDimensions(), "model") {
		t.Fatalf("model must stay in the probeable vocabulary: %q", probeableDimensions())
	}
	// The two derived dimensions are deliberately absent, and the error names
	// what is available instead of silently accepting them.
	proxy := newFakeProxy(t)
	service := newTestService(t, proxy, Limits{})
	for _, dim := range []string{"time", "error", "nope", ""} {
		_, err := service.values(context.Background(), ValuesInput{Dim: dim})
		if err == nil {
			t.Fatalf("dim %q must be refused: %q is not a stored column", dim, dim)
		}
		if !strings.Contains(err.Error(), "not probeable") {
			t.Fatalf("error = %v, want the not-probeable refusal", err)
		}
		for _, listed := range []string{"client", "provider", "tool", "error_type"} {
			if !strings.Contains(err.Error(), listed) {
				t.Fatalf("the refusal must list the probeable dimensions, got %q", err)
			}
		}
	}
	if len(proxy.requests()) != 0 {
		t.Fatal("a refused dimension must not reach the proxy")
	}
}

// TestValuesModelFoldsThroughTheProxysCanonicalization is the raw-vs-canonical
// regression. The scope filters match the canonical model name (internal/web
// fromRecord applies the configured rules), so returning raw stored spellings
// from values(model) produced a filter that silently matched zero: raw
// `glm-5.3` matched nothing while canonical `glm-5-3` matched everything. The
// probe now folds through config.ApplyModelRules - the proxy's own owner - and
// merges the counts of spellings that collapse together.
func TestValuesModelFoldsThroughTheProxysCanonicalization(t *testing.T) {
	rules, err := json.Marshal(map[string]any{"rules": config.DefaultModelRules()})
	if err != nil {
		t.Fatal(err)
	}
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, `{"seq":1,"feed_id":"f","counters":{},"storage":{"enabled":true},"records":[],`+
		`"model_canon":`+string(rules)+`}`)
	proxy.json(http.MethodGet, schemaPath, `[{"value":"glm-5.3","requests":3},`+
		`{"value":"glm-5-3","requests":2},{"value":"vendor/glm-5.3","requests":1}]`)
	service := newTestService(t, proxy, Limits{})

	out, err := service.values(context.Background(), ValuesInput{Dim: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Returned != 1 || len(out.Values) != 1 {
		t.Fatalf("the three raw spellings collapse into one canonical family: %+v", out.Values)
	}
	if out.Values[0].Value != "glm-5-3" || out.Values[0].Requests != 6 {
		t.Fatalf("values(model) = %+v, want the canonical name with the summed count", out.Values)
	}
	for _, value := range out.Values {
		if value.Value == "glm-5.3" {
			t.Fatalf("a raw stored spelling must never be returned: %+v", out.Values)
		}
	}
	if !strings.Contains(out.Note, "CANONICAL") {
		t.Fatalf("the note must say the model filter takes the canonical spelling: %q", out.Note)
	}
	// The rules travel on the bootstrap form, and the raw probe is a bounded
	// page whose ordering is the keyset order, never the final result order.
	ruleProbes := proxy.requestsFor(http.MethodGet, bootstrapPath)
	if len(ruleProbes) != 1 {
		t.Fatalf("the rule set must come from the bootstrap form: %v", proxy.requests())
	}
	assertQuery(t, ruleProbes[0], "fields", "model_canon")
	statement := proxy.requestsFor(http.MethodGet, schemaPath)[0].Query.Get("q")
	for _, needle := range []string{"model AS value", "GROUP BY value", "ORDER BY value ASC", "LIMIT "} {
		if !strings.Contains(statement, needle) {
			t.Fatalf("the model probe %q must contain %q", statement, needle)
		}
	}
}

// TestValuesReportsTruncationExactly pins the one-extra-row signal: a value
// beyond the cap is reported as withheld, and a result that fits is not.
func TestValuesReportsTruncationExactly(t *testing.T) {
	rows := `[{"value":"a","requests":3},{"value":"b","requests":2},{"value":"c","requests":1}]`
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, schemaPath, rows)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	exact, err := service.values(ctx, ValuesInput{Dim: "client", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if exact.Returned != 3 || exact.Truncation.Truncated {
		t.Fatalf("three values for a cap of three is not truncation: %+v", exact.Truncation)
	}

	clipped, err := service.values(ctx, ValuesInput{Dim: "client", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if clipped.Returned != 2 || !clipped.Truncation.Truncated || clipped.Truncation.Total != 3 {
		t.Fatalf("truncation = %+v, want 3 produced and 2 shown", clipped.Truncation)
	}
	if !strings.Contains(clipped.Truncation.Marker, "raise limit") {
		t.Fatalf("marker = %q, must name the continuation", clipped.Truncation.Marker)
	}

	if _, err := service.values(ctx, ValuesInput{Dim: "client", Limit: logPageMax + 1}); err == nil {
		t.Fatal("an absurd limit must be refused")
	}
}

// TestValuesModelPagesPastTheGenericQueryRowCap is the regression for the
// reported 413. The model dimension used to read every distinct raw spelling in
// ONE query, so a vocabulary larger than storage_query_max_rows made the tool
// unusable. Here the fake proxy enforces a 500-row cap, stricter than the
// default 10,000, and the tool must shrink its page and still enumerate 10,001
// spellings. A raw page is not the result page: the output still reports the
// exact total and truncates at the caller's limit, never at the transport cap.
func TestValuesModelPagesPastTheGenericQueryRowCap(t *testing.T) {
	const total = 10_001
	spellings := make([]string, total)
	for i := range spellings {
		spellings[i] = fmt.Sprintf("model-%05d", i)
	}
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, modelBootstrap(t, nil))
	proxy.dynamic(http.MethodGet, schemaPath, modelPageResponder(spellings, 1, 500))
	service := newTestService(t, proxy, Limits{})

	out, err := service.values(context.Background(), ValuesInput{Dim: "model", Limit: logPageMax})
	if err != nil {
		t.Fatalf("a raw vocabulary past the query cap must page, not fail: %v", err)
	}
	if out.Returned != logPageMax || len(out.Values) != logPageMax {
		t.Fatalf("returned = %d/%d, want the caller's limit %d", out.Returned, len(out.Values), logPageMax)
	}
	if !out.Truncation.Truncated || out.Truncation.Total != total {
		t.Fatalf("truncation = %+v, want all %d raw spellings counted", out.Truncation, total)
	}
	statements := proxy.requestsFor(http.MethodGet, schemaPath)
	if len(statements) < 2 {
		t.Fatalf("the model probe must page, got %d requests", len(statements))
	}
	if first := statements[0].Query.Get("q"); !strings.Contains(first, "LIMIT 1000") {
		t.Fatalf("the first page must start at the normal size: %q", first)
	}
	if last := statements[len(statements)-1].Query.Get("q"); !strings.Contains(last, "LIMIT 500") {
		t.Fatalf("after a 413 the probe must retry at half the page size: %q", last)
	}
	if !strings.Contains(out.Note, "Model enumeration reads at most") {
		t.Fatalf("the note must state the working bound: %q", out.Note)
	}
}

// TestValuesModelFoldsAcrossPagedRawSpellings proves the paging boundary does
// not change the canonical contract: 10,001 raw spellings spread over many
// pages all collapse through model_rules into one canonical name, and the
// merged count is the sum across every page.
func TestValuesModelFoldsAcrossPagedRawSpellings(t *testing.T) {
	const total = 10_001
	spellings := make([]string, total)
	for i := range spellings {
		spellings[i] = fmt.Sprintf("vendor%05d/glm-5.3", i)
	}
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, modelBootstrap(t, config.DefaultModelRules()))
	proxy.dynamic(http.MethodGet, schemaPath, modelPageResponder(spellings, 1, 0))
	service := newTestService(t, proxy, Limits{})

	out, err := service.values(context.Background(), ValuesInput{Dim: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Returned != 1 || len(out.Values) != 1 {
		t.Fatalf("the raw spellings must collapse into one canonical family: %+v", out.Values)
	}
	if out.Values[0].Value != "glm-5-3" || out.Values[0].Requests != total {
		t.Fatalf("values(model) = %+v, want glm-5-3 with the count summed across pages", out.Values)
	}
	if out.Truncation.Truncated {
		t.Fatalf("one canonical value must not report truncation: %+v", out.Truncation)
	}
}

// TestValuesModelFailsClosedWhenCanonicalVocabularyExceedsTheBound pins the
// second bound. Identity rules make every raw spelling its own canonical name,
// so the fold reaches the working-map cap. The tool must answer with an
// explicit refusal that names the canonical bound; it must not return a partial
// vocabulary and must not surface the generic query 413.
func TestValuesModelFailsClosedWhenCanonicalVocabularyExceedsTheBound(t *testing.T) {
	total := modelValuesMaxCanonical + 1
	spellings := make([]string, total)
	for i := range spellings {
		spellings[i] = fmt.Sprintf("model-%05d", i)
	}
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, modelBootstrap(t, nil))
	proxy.dynamic(http.MethodGet, schemaPath, modelPageResponder(spellings, 1, 0))
	service := newTestService(t, proxy, Limits{})

	out, err := service.values(context.Background(), ValuesInput{Dim: "model"})
	if err == nil {
		t.Fatalf("a canonical vocabulary past the bound must fail closed, got %+v", out)
	}
	if out != nil {
		t.Fatalf("a failed enumeration must not return a partial vocabulary: %+v", out)
	}
	if !strings.Contains(err.Error(), "distinct canonical model names") {
		t.Fatalf("error = %v, want the explicit canonical bound", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(modelValuesMaxCanonical)) {
		t.Fatalf("error = %v, want the bound %d named", err, modelValuesMaxCanonical)
	}
	if strings.Contains(err.Error(), "HTTP 413") {
		t.Fatalf("the canonical bound must not surface as the generic query cap: %v", err)
	}
}

// TestValuesModelFailsClosedWhenThePageBudgetIsExhausted is the work-bound
// regression. A proxy row cap of one turns every page into a single row, so a
// large vocabulary would otherwise issue one authenticated query per spelling,
// each bounded only by the request timeout. The tool must stop at the total
// page-query budget with an explicit refusal: not a long loop, and never a
// partial vocabulary that looks complete.
func TestValuesModelFailsClosedWhenThePageBudgetIsExhausted(t *testing.T) {
	total := modelValuesMaxPageQueries + 1
	spellings := make([]string, total)
	for i := range spellings {
		spellings[i] = fmt.Sprintf("model-%05d", i)
	}
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, modelBootstrap(t, nil))
	proxy.dynamic(http.MethodGet, schemaPath, modelPageResponder(spellings, 1, 1))
	service := newTestService(t, proxy, Limits{})

	out, err := service.values(context.Background(), ValuesInput{Dim: "model"})
	if err == nil {
		t.Fatalf("a one-row proxy cap with %d spellings must exceed the page budget, got %+v", total, out)
	}
	if out != nil {
		t.Fatalf("a failed enumeration must not return a partial vocabulary: %+v", out)
	}
	if !strings.Contains(err.Error(), "page queries") {
		t.Fatalf("error = %v, want the total page-query budget", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(modelValuesMaxPageQueries)) {
		t.Fatalf("error = %v, want the budget %d named", err, modelValuesMaxPageQueries)
	}
	if strings.Contains(err.Error(), "HTTP 413") {
		t.Fatalf("the page budget must not surface as the generic query cap: %v", err)
	}
	queries := proxy.requestsFor(http.MethodGet, schemaPath)
	if len(queries) != modelValuesMaxPageQueries {
		t.Fatalf("the probe sent %d page queries, want it to stop at the budget %d", len(queries), modelValuesMaxPageQueries)
	}
}

// TestValuesModelEnumeratesWithinThePageBudget pins the other side of the
// bound: an ordinary multi-page vocabulary and a vocabulary of exactly one
// full page both finish, and the full page is followed by the empty page that
// proves it is the end rather than assumed to be complete.
func TestValuesModelEnumeratesWithinThePageBudget(t *testing.T) {
	cases := []struct {
		name        string
		total       int
		wantQueries int
	}{
		{name: "multi page", total: 2_500, wantQueries: 3},
		{name: "exactly one full page", total: modelValuesPageSize, wantQueries: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spellings := make([]string, tc.total)
			for i := range spellings {
				spellings[i] = fmt.Sprintf("model-%05d", i)
			}
			proxy := newFakeProxy(t)
			proxy.json(http.MethodGet, bootstrapPath, modelBootstrap(t, nil))
			proxy.dynamic(http.MethodGet, schemaPath, modelPageResponder(spellings, 1, 0))
			service := newTestService(t, proxy, Limits{})

			out, err := service.values(context.Background(), ValuesInput{Dim: "model"})
			if err != nil {
				t.Fatalf("a vocabulary inside the page budget must enumerate: %v", err)
			}
			if out.Truncation.Total != tc.total {
				t.Fatalf("total = %d, want every one of the %d spellings folded", out.Truncation.Total, tc.total)
			}
			if !strings.Contains(out.Note, strconv.Itoa(modelValuesMaxPageQueries)+" page queries") {
				t.Fatalf("the note must state the page-query bound: %q", out.Note)
			}
			queries := proxy.requestsFor(http.MethodGet, schemaPath)
			if len(queries) != tc.wantQueries {
				t.Fatalf("page queries = %d, want %d", len(queries), tc.wantQueries)
			}
			if len(queries) > modelValuesMaxPageQueries {
				t.Fatalf("a normal vocabulary must stay inside the page budget: %d > %d", len(queries), modelValuesMaxPageQueries)
			}
		})
	}
}

// modelBootstrap composes the bootstrap form the model fold reads. nil rules
// are the identity pipeline, the same as an empty list.
func modelBootstrap(t *testing.T, rules []config.ModelRule) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"seq": 1, "feed_id": "f", "counters": map[string]any{},
		"storage": map[string]any{"enabled": true}, "records": []any{},
		"model_canon": map[string]any{"rules": rules},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// modelPageResponder serves one sorted raw spelling list page by page. It
// reads the keyset cursor and LIMIT out of the statement the tool built, and
// optional rowCap simulates the proxy's storage_query_max_rows refusal.
func modelPageResponder(spellings []string, count int64, rowCap int) func(*http.Request) cannedResponse {
	return func(r *http.Request) cannedResponse {
		after, limit, err := parseModelPageStatement(r.URL.Query().Get("q"))
		if err != nil {
			return cannedResponse{Status: http.StatusInternalServerError, ContentType: "application/json", Body: `{"error":"` + err.Error() + `"}`}
		}
		if rowCap > 0 && limit > rowCap {
			return cannedResponse{Status: http.StatusRequestEntityTooLarge, ContentType: "application/json",
				Body: `{"error":"query result exceeds storage_query_max_rows"}`}
		}
		start := 0
		if after != nil {
			start = sort.SearchStrings(spellings, *after)
			if start < len(spellings) && spellings[start] == *after {
				start++
			}
		}
		end := min(start+limit, len(spellings))
		var body strings.Builder
		body.WriteByte('[')
		for i := start; i < end; i++ {
			if i > start {
				body.WriteByte(',')
			}
			value, err := json.Marshal(spellings[i])
			if err != nil {
				return cannedResponse{Status: http.StatusInternalServerError, ContentType: "application/json", Body: `{"error":"encode fixture"}`}
			}
			fmt.Fprintf(&body, `{"value":%s,"requests":%d}`, value, count)
		}
		body.WriteByte(']')
		return cannedResponse{Status: http.StatusOK, ContentType: "application/json", Body: body.String()}
	}
}

// parseModelPageStatement reads the closed statement modelPageStatement emits
// in tests. The cursor is a hex-encoded TEXT expression, so this decodes the
// exact bytes instead of reading a quoted literal.
func parseModelPageStatement(statement string) (*string, int, error) {
	const afterMarker = " WHERE model > CAST(X'"
	const afterSuffix = "' AS TEXT)"
	var after *string
	if start := strings.Index(statement, afterMarker); start >= 0 {
		rest := statement[start+len(afterMarker):]
		end := strings.Index(rest, afterSuffix)
		if end < 0 {
			return nil, 0, fmt.Errorf("unterminated model cursor in %q", statement)
		}
		decoded, err := hex.DecodeString(rest[:end])
		if err != nil {
			return nil, 0, fmt.Errorf("invalid model cursor hex in %q: %w", statement, err)
		}
		cursor := string(decoded)
		after = &cursor
	}
	const limitMarker = " LIMIT "
	start := strings.LastIndex(statement, limitMarker)
	if start < 0 {
		return nil, 0, fmt.Errorf("missing LIMIT in %q", statement)
	}
	limit, err := strconv.Atoi(statement[start+len(limitMarker):])
	if err != nil || limit < 1 {
		return nil, 0, fmt.Errorf("invalid LIMIT in %q", statement)
	}
	return after, limit, nil
}

// hostileModelSpellings are legal stored model names that also spell SQL
// structure, denied statement-gate keywords, quoting edge cases or non-ASCII
// and NUL bytes. A client controls the stored spelling, so the model cursor
// must carry every one of them as data rather than as SQL source.
var hostileModelSpellings = []string{
	"", "O'Brien", "a;b", "attach", "attaché", "back\\slash",
	"load_extension", "nul\x00byte", "pragma", "日本語",
}

// fakeStatementGate mirrors the proxy storage gate's observable contract
// without depending on its unexported implementation: one SELECT-leading
// statement, no ';', and none of the denied keywords anywhere in the text.
// It is deliberately stricter than storage's word-boundary check (a plain
// substring match), so a statement that spells any denied word in any context
// fails here. The real storage gate is exercised separately against a temp
// database below.
func fakeStatementGate(statement string) error {
	trimmed := strings.TrimSpace(statement)
	if !strings.HasPrefix(strings.ToUpper(trimmed), "SELECT") {
		return fmt.Errorf("statement is not a single SELECT")
	}
	if strings.Contains(trimmed, ";") {
		return fmt.Errorf("statement contains a statement terminator")
	}
	upper := strings.ToUpper(trimmed)
	for _, keyword := range []string{"ATTACH", "DETACH", "PRAGMA", "LOAD_EXTENSION"} {
		if strings.Contains(upper, keyword) {
			return fmt.Errorf("statement contains the denied keyword %s", keyword)
		}
	}
	return nil
}

// TestValuesModelHostileCursorsStayOutOfSQL pins the statement source: every
// legal cursor, including gate keywords and statement terminators, is carried
// as a hex TEXT expression and never appears in the SQL text. The empty cursor
// is still expressible as an empty hex literal; nil remains the no-cursor
// first page.
func TestValuesModelHostileCursorsStayOutOfSQL(t *testing.T) {
	if first := modelPageStatement(nil, 11); strings.Contains(first, " WHERE ") {
		t.Fatalf("the first model page must not carry a cursor: %q", first)
	}
	for _, cursor := range hostileModelSpellings {
		statement := modelPageStatement(&cursor, 11)
		if err := fakeStatementGate(statement); err != nil {
			t.Fatalf("cursor %q generated a statement the gate rejects: %v (%q)", cursor, err, statement)
		}
		if !strings.Contains(statement, "CAST(X'") || !strings.Contains(statement, "' AS TEXT)") {
			t.Fatalf("cursor %q must be a cast hex text expression: %q", cursor, statement)
		}
		if cursor != "" && strings.Contains(statement, cursor) {
			t.Fatalf("cursor %q leaked into the statement source: %q", cursor, statement)
		}
		decoded, limit, err := parseModelPageStatement(statement)
		if err != nil {
			t.Fatalf("cursor %q: %v", cursor, err)
		}
		if decoded == nil || *decoded != cursor {
			t.Fatalf("decoded cursor = %v, want %q from %q", decoded, cursor, statement)
		}
		if limit != 11 {
			t.Fatalf("decoded limit = %d, want 11 from %q", limit, statement)
		}
	}
}

// TestValuesModelHostileSpellingsPageThroughRealStorage drives the whole fold
// against a temp SQLite database through storage's real HandleQuery gate. Every
// generated page must pass that gate and the final canonical vocabulary must be
// complete: a missing page or a duplicated cursor would change the total or
// shift the returned prefix. The page boundary lands on a spelling ending in
// "-attach", so the old raw quoted cursor was denied by the real gate.
func TestValuesModelHostileSpellingsPageThroughRealStorage(t *testing.T) {
	const generated = modelValuesPageSize + 5
	spellings := make([]string, 0, generated+len(hostileModelSpellings))
	for i := 0; i < generated; i++ {
		spellings = append(spellings, fmt.Sprintf("model-%04d-attach", i))
	}
	spellings = append(spellings, hostileModelSpellings...)

	store, path := openModelStorage(t)
	seedModelSpellings(t, path, spellings)
	proxy := newStorageModelProxy(t, store)
	service := newTestService(t, proxy, Limits{})

	out, err := service.values(context.Background(), ValuesInput{Dim: "model", Limit: logPageMax})
	if err != nil {
		t.Fatalf("hostile stored spellings must page through the real query gate: %v", err)
	}
	if out.Returned != logPageMax || len(out.Values) != logPageMax {
		t.Fatalf("returned = %d/%d, want the caller's limit %d", out.Returned, len(out.Values), logPageMax)
	}
	if !out.Truncation.Truncated || out.Truncation.Total != len(spellings) {
		t.Fatalf("truncation = %+v, want all %d spellings counted", out.Truncation, len(spellings))
	}
	want := append([]string(nil), spellings...)
	sort.Strings(want)
	for i, value := range out.Values {
		if value.Value != want[i] || value.Requests != 1 {
			t.Fatalf("values[%d] = %+v, want %q with one request", i, value, want[i])
		}
	}
	requests := proxy.requestsFor(http.MethodGet, schemaPath)
	if len(requests) < 2 {
		t.Fatalf("the vocabulary must span more than one page, got %d requests", len(requests))
	}
	for _, request := range requests {
		statement := request.Query.Get("q")
		if err := fakeStatementGate(statement); err != nil {
			t.Fatalf("a generated page fails the gate: %v (%q)", err, statement)
		}
		if strings.Contains(statement, " WHERE model >") && !strings.Contains(statement, "CAST(X'") {
			t.Fatalf("a paged statement must carry its cursor as hex text: %q", statement)
		}
	}
}

// TestValuesModelExactlyPageSizeEndsOnHostileSpelling is the boundary
// regression: a full page makes the pager ask for one more page, and the
// cursor is the last spelling. When that spelling is hostile the extra empty
// page used to fail with HTTP 400, so a complete vocabulary of exactly one page
// was unreadable. The follow-up page must pass the real gate and return empty.
func TestValuesModelExactlyPageSizeEndsOnHostileSpelling(t *testing.T) {
	spellings := make([]string, 0, modelValuesPageSize)
	for i := 0; i < modelValuesPageSize-1; i++ {
		spellings = append(spellings, fmt.Sprintf("model-%04d", i))
	}
	spellings = append(spellings, "zz-attach")

	store, path := openModelStorage(t)
	seedModelSpellings(t, path, spellings)
	proxy := newStorageModelProxy(t, store)
	service := newTestService(t, proxy, Limits{})

	out, err := service.values(context.Background(), ValuesInput{Dim: "model", Limit: logPageMax})
	if err != nil {
		t.Fatalf("a full page ending on a hostile spelling must not fail the empty follow-up page: %v", err)
	}
	if !out.Truncation.Truncated || out.Truncation.Total != modelValuesPageSize {
		t.Fatalf("truncation = %+v, want all %d spellings counted", out.Truncation, modelValuesPageSize)
	}
	requests := proxy.requestsFor(http.MethodGet, schemaPath)
	if len(requests) != 2 {
		t.Fatalf("the exactly-full vocabulary must take one full page and one empty page, got %d requests", len(requests))
	}
	followUp := requests[1].Query.Get("q")
	if !strings.Contains(followUp, "CAST(X'7a7a2d617474616368' AS TEXT)") {
		t.Fatalf("the follow-up page must keyset on zz-attach as hex text: %q", followUp)
	}
	if err := fakeStatementGate(followUp); err != nil {
		t.Fatalf("the follow-up page fails the gate: %v (%q)", err, followUp)
	}
}

// openModelStorage opens the real durable store in a temp directory using the
// canonical config defaults, so the test runs the same query limits production
// does instead of a locally invented policy.
func openModelStorage(t *testing.T) (*storage.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model-values.db")
	defaults := config.Default()
	store, err := storage.Open(path, storage.Options{
		WriteChanCap:  defaults.StorageWriteChanCap,
		BatchCap:      defaults.StorageBatchCap,
		FlushInterval: defaults.StorageFlushInterval,
		QueryTimeout:  defaults.StorageQueryTimeout,
		QueryMaxBytes: int(defaults.StorageQueryMaxBytes),
		QueryMaxRows:  defaults.StorageQueryMaxRows,
	})
	if err != nil {
		t.Fatalf("open temp storage: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close temp storage: %v", err)
		}
	})
	return store, path
}

// newStorageModelProxy serves the model fold's bootstrap rules and routes the
// SQL read through storage's real HandleQuery, which owns the production
// statement gate, execution and result caps.
func newStorageModelProxy(t *testing.T, store *storage.Store) *fakeProxy {
	t.Helper()
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, modelBootstrap(t, nil))
	proxy.dynamic(http.MethodGet, schemaPath, func(r *http.Request) cannedResponse {
		rec := httptest.NewRecorder()
		store.HandleQuery(rec, r)
		return cannedResponse{
			Status:      rec.Code,
			ContentType: rec.Header().Get("Content-Type"),
			Body:        rec.Body.String(),
		}
	})
	return proxy
}

// seedModelSpellings inserts one requests row per raw model spelling directly
// into the initialized temp database. Every NOT NULL column without a default
// is named, following storage's own test fixture.
func seedModelSpellings(t *testing.T, path string, spellings []string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	insert, err := tx.Prepare(`INSERT INTO requests (
		id, provider, model, key_hash, user_agent, stream, status_code,
		started_at, duration_ms, ttft_ms, first_token_at, last_token_at,
		finish_reason, error_type, error_msg, error_code, tool_calls,
		input_tokens, output_tokens, total_tokens, cache_read_tokens,
		cache_write_tokens, reasoning_tokens, client_disconnected,
		rate_limit_remaining, rate_limit_limit, cost,
		turns_user, turns_assistant, turns_tool, response_headers
	) VALUES (?, 'neutral.example', ?, 'k', 'ua', 0, 200, ?, 0, 0, 0, 0,
		'', '', '', '', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, '')`)
	if err != nil {
		t.Fatal(err)
	}
	for i, spelling := range spellings {
		if _, err := insert.Exec(fmt.Sprintf("model-value-%05d", i), spelling, i); err != nil {
			t.Fatalf("seed spelling %q: %v", spelling, err)
		}
	}
	if err := insert.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
