package mcp

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// valuesDimensions maps every probeable filter dimension to the statement that
// enumerates it. It is a closed set of hardcoded statements, never a formatted
// user string: the caller names a key here, so no dimension, column or clause
// can come from the model. A dimension that is NOT here has no stored column to
// read - `time` is a Go-owned daypart bucket and `error` is a Go-owned
// type|code|message composite - so both are documented in describe instead of
// being faked in SQL, which would be a second owner drifting from the first.
var valuesDimensions = map[string]func(limit int) string{
	"client":        func(limit int) string { return countStatement("client", "", limit) },
	"provider":      func(limit int) string { return countStatement("provider", "", limit) },
	"model":         func(limit int) string { return countStatement("model", "", limit) },
	"conversation":  func(limit int) string { return countStatement("conversation_id", "", limit) },
	"key":           func(limit int) string { return countStatement("key_hash", "", limit) },
	"status":        func(limit int) string { return countStatement("CAST(status_code AS TEXT)", "", limit) },
	"error_type":    func(limit int) string { return countStatement("error_type", "error_type != ''", limit) },
	"error_code":    func(limit int) string { return countStatement("error_code", "error_code != ''", limit) },
	"error_message": func(limit int) string { return countStatement("error_msg", "error_msg != ''", limit) },
	"tool": func(limit int) string {
		// Tool names live in a JSON-array column, one array per request, so the
		// enumeration walks it rather than grouping the raw text.
		return "SELECT j.value AS value, COUNT(*) AS requests FROM requests r, json_each(r.tool_names) j" +
			" WHERE j.value != '' GROUP BY j.value ORDER BY requests DESC, value ASC LIMIT " + strconv.Itoa(limit)
	},
}

// countStatement is the one shape every column-valued probe uses. value and
// where are trusted constants from the table above, never model input.
func countStatement(value, where string, limit int) string {
	statement := "SELECT " + value + " AS value, COUNT(*) AS requests FROM requests"
	if where != "" {
		statement += " WHERE " + where
	}
	return statement + " GROUP BY value ORDER BY requests DESC, value ASC LIMIT " + strconv.Itoa(limit)
}

// probeableDimensions is the error message's vocabulary, in a stable order.
func probeableDimensions() string {
	return strings.Join(slices.Sorted(maps.Keys(valuesDimensions)), ", ")
}

// ValuesInput enumerates one dimension's vocabulary.
type ValuesInput struct {
	// Dim is the dimension whose values the caller needs to discover.
	Dim string `json:"dim" jsonschema:"one of: client, provider, model, conversation, key, status, tool, error_type, error_code, error_message"`
	// Limit caps how many values are returned. 0 uses this server's default.
	Limit int `json:"limit,omitempty" jsonschema:"optional cap on returned values; values beyond it are reported as truncated, never silently dropped"`
}

// ValueCount is one observed value and how many requests carried it.
type ValueCount struct {
	Value    string `json:"value" jsonschema:"the exact stored value, which is what a dim:id filter must name"`
	Requests int64  `json:"requests" jsonschema:"requests in durable history carrying this value"`
}

// ValuesOutput is one dimension's vocabulary, most frequent first.
type ValuesOutput struct {
	Dim        string       `json:"dim" jsonschema:"the dimension that was enumerated"`
	Values     []ValueCount `json:"values" jsonschema:"distinct values, most frequent first"`
	Returned   int          `json:"returned" jsonschema:"values returned after truncation"`
	Truncation Truncation   `json:"truncation" jsonschema:"whether values were withheld"`
	Note       string       `json:"note" jsonschema:"what this vocabulary is, and what a filter on it accepts"`
}

// valuesNote is the standing caveat: a mistyped filter value is an EMPTY SUCCESS
// from explore, chart and records, never an error, so discovering the vocabulary
// first is what keeps a confidently wrong answer from looking right. It also
// states the two dimensions this tool cannot enumerate, and why.
const valuesNote = "these are the exact stored values a dim:id filter must name; an unknown value is an EMPTY result, not an error, " +
	"so check here before filtering. Cross-filtered counts (one dimension under a filter on another) need query with a GROUP BY. " +
	"model values are raw spellings: records and chart publish the model_canon map that folds them into canonical families. " +
	"The status values are raw codes; the status FILTER takes the status classes describe lists, never a code. " +
	"time and error are not listed here: the time buckets and the error type|code|message keys are derived by the proxy, not stored"

// Values lists the distinct values of one dimension over durable history. It
// exists because the filter vocabularies are otherwise undiscoverable: a
// misspelled client, provider, model, tool, conversation or key is an empty
// success from every scoped tool, so a model that guesses a value reports a
// confident zero instead of a correctable error.
func (s *Service) values(ctx context.Context, in ValuesInput) (*ValuesOutput, error) {
	build, known := valuesDimensions[in.Dim]
	if !known {
		return nil, fmt.Errorf("dim %q is not probeable; use one of %s", in.Dim, probeableDimensions())
	}
	limit := in.Limit
	if limit <= 0 {
		limit = s.limits.PageSize
	}
	if limit > logPageMax {
		return nil, fmt.Errorf("limit must be at most %d", logPageMax)
	}
	// One extra row is the exact truncation signal: a value exists beyond the
	// cap, without guessing from a suspiciously full page.
	rows, err := s.client.querySQL(ctx, build(limit+1))
	if err != nil {
		return nil, err
	}
	found := make([]ValueCount, 0, len(rows))
	for _, row := range rows {
		found = append(found, ValueCount{Value: rowString(row, "value"), Requests: rowInt64(row, "requests")})
	}
	kept, truncation := clamp(found, limit, "values", "raise limit, or ask query for a GROUP BY over the same column")
	return &ValuesOutput{Dim: in.Dim, Values: kept, Returned: len(kept), Truncation: truncation, Note: valuesNote}, nil
}
