package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"
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
		"model":         {"model AS value", "FROM requests"},
		"conversation":  {"conversation_id AS value", "FROM requests"},
		"key":           {"key_hash AS value", "FROM requests"},
		"status":        {"CAST(status_code AS TEXT) AS value", "FROM requests"},
		"error_type":    {"error_type AS value", "error_type != ''"},
		"error_code":    {"error_code AS value", "error_code != ''"},
		"error_message": {"error_msg AS value", "error_msg != ''"},
		"tool":          {"json_each(r.tool_names)", "j.value != ''", "AS value"},
	}
	if len(valuesDimensions) != len(want) {
		t.Fatalf("probeable dimensions = %d, want %d", len(valuesDimensions), len(want))
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
