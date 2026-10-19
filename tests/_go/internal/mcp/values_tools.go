package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
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
	if !strings.Contains(paged, "WHERE model > 'glm-5-3'") {
		t.Fatalf("the model page must keyset after the last spelling: %q", paged)
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
// in tests. The fixture spellings contain no quotes.
func parseModelPageStatement(statement string) (*string, int, error) {
	const afterMarker = " WHERE model > '"
	var after *string
	if start := strings.Index(statement, afterMarker); start >= 0 {
		rest := statement[start+len(afterMarker):]
		end := strings.Index(rest, "'")
		if end < 0 {
			return nil, 0, fmt.Errorf("unterminated model cursor in %q", statement)
		}
		cursor := rest[:end]
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
