package mcp

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// valuesDimensions maps every probeable filter dimension to the statement that
// enumerates it. It is a closed set of hardcoded statements, never a formatted
// user string: the caller names a key here, so no dimension, column or clause
// can come from the model. A dimension that is NOT here has no stored column to
// read - `time` is a Go-owned daypart bucket and `error` is a Go-owned
// type|code|message composite - so both are documented in describe instead of
// being faked in SQL, which would be a second owner drifting from the first.
//
// model is the one dimension whose rows are folded before they are returned:
// the SQL enumerates the RAW stored spellings with no LIMIT, and values()
// folds and merges them through the proxy's own rule pipeline. A raw-level
// LIMIT is not valid there because the cap applies to the canonical result.
var valuesDimensions = map[string]func(limit int) string{
	"client":        func(limit int) string { return countStatement("client", "", limit) },
	"provider":      func(limit int) string { return countStatement("provider", "", limit) },
	"model":         func(limit int) string { return countStatement("model", "", 0) },
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
// where are trusted constants from the table above, never model input. A
// non-positive limit omits the LIMIT clause, for the one probe whose cap is
// applied after a client-side fold.
func countStatement(value, where string, limit int) string {
	statement := "SELECT " + value + " AS value, COUNT(*) AS requests FROM requests"
	if where != "" {
		statement += " WHERE " + where
	}
	statement += " GROUP BY value ORDER BY requests DESC, value ASC"
	if limit > 0 {
		statement += " LIMIT " + strconv.Itoa(limit)
	}
	return statement
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
	Value    string `json:"value" jsonschema:"the filter value to name; model values are canonical spellings"`
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
const valuesNote = "these are the values a dim:id filter must name exactly; an unknown value is an EMPTY result, not an error, " +
	"so check here before filtering. Cross-filtered counts (one dimension under a filter on another) need query with a GROUP BY. " +
	"model values are CANONICAL spellings, folded through the proxy's configured model_rules: the explore, chart and records " +
	"filters match the canonical name, so a raw stored spelling such as one with a vendor/ prefix or a dot instead of a dash matches nothing. " +
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
	if in.Dim == "model" {
		return s.modelValues(ctx, limit)
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

// modelValues enumerates the CANONICAL model vocabulary. The stored model
// column is the raw spelling, but the explorer, chart and records filters match
// the canonical name the proxy derives (internal/web fromRecord), so a raw
// spelling returned here would be a filter that silently matches zero. The rule
// set travels on the full bootstrap form; this folds every raw spelling through
// the same config.ApplyModelRules pipeline and merges the counts, so the
// vocabulary returned is the vocabulary a filter accepts.
//
// Every distinct raw spelling is read, because the cap applies to the merged
// canonical names: a raw-level LIMIT could drop spellings whose summed count
// belongs in the returned page.
func (s *Service) modelValues(ctx context.Context, limit int) (*ValuesOutput, error) {
	exec, err := s.client.modelRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the proxy's model canonicalization rules before enumerating models: %v", err)
	}
	rows, err := s.client.querySQL(ctx, valuesDimensions["model"](0))
	if err != nil {
		return nil, err
	}
	kept, truncation := clamp(foldModelValues(rows, exec), limit, "values", "raise limit, or ask query for a GROUP BY over the same column")
	return &ValuesOutput{Dim: "model", Values: kept, Returned: len(kept), Truncation: truncation, Note: valuesNote}, nil
}

// foldModelValues folds each raw spelling through the compiled pipeline and
// sums the counts of spellings that collapse into one canonical name, in the
// same most-frequent-first order the SQL probes use.
func foldModelValues(rows rows, exec []config.ModelRuleExec) []ValueCount {
	counts := map[string]int64{}
	for _, row := range rows {
		canonical := config.ApplyModelRules(exec, rowString(row, "value"))
		counts[canonical] += rowInt64(row, "requests")
	}
	out := make([]ValueCount, 0, len(counts))
	for value, requests := range counts {
		out = append(out, ValueCount{Value: value, Requests: requests})
	}
	slices.SortFunc(out, func(a, b ValueCount) int {
		if a.Requests != b.Requests {
			return cmp.Compare(b.Requests, a.Requests)
		}
		return strings.Compare(a.Value, b.Value)
	})
	return out
}
