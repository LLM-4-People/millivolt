package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// modelDimension is the one probeable dimension whose values are folded rather
// than returned as stored. It has a paged probe (modelPageStatement) instead of
// an entry in valuesDimensions.
const modelDimension = "model"

// valuesDimensions maps every NON-MODEL probeable filter dimension to the
// statement that enumerates it. It is a closed set of hardcoded statements,
// never a formatted user string: the caller names a key here, so no dimension,
// column or clause can come from the model. A dimension that is NOT here has no
// stored column to read - `time` is a Go-owned daypart bucket and `error` is a
// Go-owned type|code|message composite - so both are documented in describe
// instead of being faked in SQL, which would be a second owner drifting from
// the first. model is documented separately because its probe is paged.
var valuesDimensions = map[string]func(limit int) string{
	"client":        func(limit int) string { return countStatement("client", "", limit) },
	"provider":      func(limit int) string { return countStatement("provider", "", limit) },
	"conversation":  func(limit int) string { return countStatement("conversation_id", "", limit) },
	"key":           func(limit int) string { return countStatement("key_hash", "", limit) },
	"status":        func(limit int) string { return countStatement("CAST(status_code AS TEXT)", "", limit) },
	"error_type":    func(limit int) string { return countStatement("error_type", "error_type != ''", limit) },
	"error_code":    func(limit int) string { return countStatement("error_code", "error_code != ''", limit) },
	"error_message": func(limit int) string { return countStatement("error_msg", "error_msg != ''", limit) },
	"tool": func(limit int) string {
		// Tool names live in a JSON-array column, one array per request, so the
		// enumeration walks it rather than grouping the raw text. A legacy
		// empty spelling (or any malformed value) is no vocabulary, never a
		// malformed-JSON error: the CASE keeps json_each's input valid. One
		// request contributes once per distinct name even when its array
		// repeats a name, so COUNT(DISTINCT r.id) is the request count.
		return "SELECT j.value AS value, COUNT(DISTINCT r.id) AS requests FROM requests r," +
			" json_each(CASE WHEN json_valid(r.tool_names) THEN r.tool_names ELSE '[]' END) j" +
			" WHERE j.value != '' GROUP BY j.value ORDER BY requests DESC, value ASC LIMIT " + strconv.Itoa(limit)
	},
}

// countStatement is the one shape every column-valued probe uses. value and
// where are trusted constants from the table above, never model input, and
// limit is always a positive bounded page.
func countStatement(value, where string, limit int) string {
	statement := "SELECT " + value + " AS value, COUNT(*) AS requests FROM requests"
	if where != "" {
		statement += " WHERE " + where
	}
	return statement + " GROUP BY value ORDER BY requests DESC, value ASC LIMIT " + strconv.Itoa(limit)
}

// modelPageStatement enumerates one bounded page of RAW stored model
// spellings, ordered by spelling so a keyset cursor can advance without
// OFFSET. after nil starts at the lowest spelling (including the empty one);
// otherwise the page starts strictly after the last spelling of the previous
// page. The cursor is client-controlled text carried as a hex-encoded TEXT
// expression, never as raw SQL source: a legal spelling such as "attach" or
// "a;b" must not trip the proxy's statement gate. The raw spelling is only a
// paging key: values() folds every page through the proxy's canonicalization
// rules and merges counts before applying the caller's limit, so a spelling
// that collapses into a returned canonical name is never dropped by the page
// bound.
//
// The generic query route caps a result at storage_query_max_rows, which is a
// deployment setting this client cannot read. A single unpaged SELECT DISTINCT
// therefore turned a large vocabulary into HTTP 413. Each page here is well
// below the default cap and the caller halves it when the proxy still answers
// 413, so the generic cap bounds a page instead of making the dimension
// unreadable.
func modelPageStatement(after *string, limit int) string {
	statement := "SELECT model AS value, COUNT(*) AS requests FROM requests"
	if after != nil {
		statement += " WHERE model > " + sqlHexText(*after)
	}
	return statement + " GROUP BY value ORDER BY value ASC LIMIT " + strconv.Itoa(limit)
}

// Model vocabulary bounds. These are internal safety limits, not configurable
// values: the point is that one dimension whose spelling is attacker-controlled
// cannot make the tool allocate or grind without bound. A page is folded and
// discarded, so only canonical names and their counts are retained.
const (
	// modelValuesPageSize is the keyset page this tool asks for. It is far
	// below the default storage_query_max_rows (10,000) and is halved on a 413
	// so a stricter deployment still works.
	modelValuesPageSize = 1000
	// modelValuesMaxRawSpellings bounds the raw vocabulary the fold walks even
	// when many spellings collapse into one canonical name. Without it, a
	// hostile client could keep the pager busy with an unbounded number of
	// spellings that all merge into a single returned value.
	modelValuesMaxRawSpellings = 100_000
	// modelValuesMaxPageQueries bounds the total proxy queries one enumeration
	// may spend, including the retries that halve a page after a 413. The raw
	// spelling bound alone is not a work bound: when the proxy's row cap forces
	// one-row pages, 100,000 spellings would mean 100,000 authenticated queries,
	// each bounded only by the request timeout. At the normal 1,000-row page a
	// full raw vocabulary needs 101 queries, and a 500-row proxy cap needs 202,
	// so both stay inside this budget; a stricter cap or larger vocabulary
	// fails closed instead of grinding.
	modelValuesMaxPageQueries = 256
	// modelValuesMaxCanonical bounds the merged working map and therefore the
	// tool's memory. A vocabulary past it fails closed instead of returning a
	// partial list, because a partial model vocabulary would silently turn a
	// valid filter into an empty result.
	modelValuesMaxCanonical = 20_000
	// modelValuesMaxCanonicalBytes bounds the sum of the retained canonical
	// spellings. The count cap alone still allows 20,000 very long names.
	modelValuesMaxCanonicalBytes = 4 << 20
)

// probeableDimensions is the error message's vocabulary, in a stable order.
// model is included even though it is not in valuesDimensions: it is still a
// stored, filterable dimension, just one with a paged probe.
func probeableDimensions() string {
	dimensions := append(slices.Sorted(maps.Keys(valuesDimensions)), modelDimension)
	slices.Sort(dimensions)
	return strings.Join(dimensions, ", ")
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
	limit, err := resolveLimit("limit", in.Limit, s.limits.PageSize, 1, logPageMax)
	if err != nil {
		return nil, err
	}
	if in.Dim == modelDimension {
		return s.modelValues(ctx, limit)
	}
	build, known := valuesDimensions[in.Dim]
	if !known {
		return nil, fmt.Errorf("dim %q is not probeable; use one of %s", in.Dim, probeableDimensions())
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
// The raw vocabulary is read in bounded keyset pages. The cap applies to the
// merged canonical names, so a raw-level page is not the result cap: every page
// is folded and discarded, and a spelling that collapses into a returned
// canonical name contributes its count no matter which page carried it. The
// generic query route's storage_query_max_rows is therefore never asked to
// return the whole raw vocabulary in one result.
//
// The pages are separate reads, so a write committed between them can race the
// walk. That is acceptable for a discovery surface: a filter value is either
// present in the history the walk observed or it is not, and the same
// best-effort contract already applies to every other vocabulary probe.
func (s *Service) modelValues(ctx context.Context, limit int) (*ValuesOutput, error) {
	exec, err := s.client.modelRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the proxy's model canonicalization rules before enumerating models: %v", err)
	}
	vocabulary := newModelVocabulary()
	pageSize := modelValuesPageSize
	var after *string
	queries := 0
	for {
		// Every page, including each 413 retry, is one authenticated proxy
		// query. Count them all: with a one-row proxy cap the raw-spelling
		// bound alone would still allow one query per spelling.
		if queries >= modelValuesMaxPageQueries {
			return nil, fmt.Errorf("model vocabulary is too large to enumerate safely: it needs more than %d proxy page queries; "+
				"the proxy's storage_query_max_rows is too small for this vocabulary or the vocabulary is too large - "+
				"inspect raw spellings with query over a WHERE started_at range and GROUP BY model", modelValuesMaxPageQueries)
		}
		queries++
		page, err := s.client.querySQL(ctx, modelPageStatement(after, pageSize))
		if err != nil {
			// The proxy's row or byte cap is a deployment setting this client
			// cannot read. A 413 means this page is too large; shrink it and
			// retry the same cursor. One row per page is the floor: if even
			// that is over a byte cap, report the failure instead of pretending
			// the vocabulary is empty or complete.
			if pageSize > 1 && queryPageTooLarge(err) {
				pageSize /= 2
				continue
			}
			return nil, fmt.Errorf("enumerate model spellings: %w", err)
		}
		for _, row := range page {
			if err := vocabulary.add(rowString(row, "value"), rowInt64(row, "requests"), exec); err != nil {
				return nil, err
			}
		}
		if len(page) < pageSize {
			break
		}
		last := rowString(page[len(page)-1], "value")
		if after != nil && last == *after {
			// A full page that ends on the cursor means the cursor did not
			// advance. Refuse it instead of folding the same rows forever.
			return nil, fmt.Errorf("model spelling page did not advance past %q", last)
		}
		after = &last
	}
	kept, truncation := clamp(vocabulary.values(), limit, "values", "raise limit, or ask query for a GROUP BY over the same column")
	return &ValuesOutput{Dim: modelDimension, Values: kept, Returned: len(kept), Truncation: truncation, Note: modelValuesNote()}, nil
}

// modelVocabulary accumulates folded model counts across bounded raw pages.
// Only canonical names and their counts are retained, so the working set is
// bounded by the canonical vocabulary rather than by the raw spelling count.
type modelVocabulary struct {
	counts map[string]int64
	bytes  int
	raw    int
}

func newModelVocabulary() *modelVocabulary {
	return &modelVocabulary{counts: make(map[string]int64)}
}

// add folds one raw spelling and merges its count. It fails closed at either
// bound: a partial vocabulary would be indistinguishable from a complete one,
// and a filter on a missing canonical name is an empty success.
func (v *modelVocabulary) add(raw string, requests int64, exec []config.ModelRuleExec) error {
	v.raw++
	if v.raw > modelValuesMaxRawSpellings {
		return fmt.Errorf("model vocabulary is too large to enumerate safely: more than %d distinct raw spellings; "+
			"inspect raw spellings with query over a WHERE started_at range and GROUP BY model", modelValuesMaxRawSpellings)
	}
	canonical := config.ApplyModelRules(exec, raw)
	if _, seen := v.counts[canonical]; !seen {
		if len(v.counts) >= modelValuesMaxCanonical {
			return fmt.Errorf("model vocabulary is too large to enumerate safely: more than %d distinct canonical model names after model_rules; "+
				"add model_rules that collapse more spellings, or inspect raw spellings with query over a WHERE started_at range and GROUP BY model", modelValuesMaxCanonical)
		}
		if v.bytes+len(canonical) > modelValuesMaxCanonicalBytes {
			return fmt.Errorf("model vocabulary is too large to enumerate safely: canonical model names exceed the %d byte bound; "+
				"add model_rules that collapse more spellings, or inspect raw spellings with query over a WHERE started_at range and GROUP BY model", modelValuesMaxCanonicalBytes)
		}
		v.bytes += len(canonical)
	}
	v.counts[canonical] += requests
	return nil
}

// values extracts the folded counts in the same most-frequent-first order the
// other probes use.
func (v *modelVocabulary) values() []ValueCount {
	out := make([]ValueCount, 0, len(v.counts))
	for value, requests := range v.counts {
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

// queryPageTooLarge reports the proxy's explicit result-size refusal. It is
// kept here, next to the one caller that can react to it, instead of turning
// every read error into a retry.
func queryPageTooLarge(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusRequestEntityTooLarge
}

// modelValuesNote appends the model-only working bound to the shared note. The
// bounds are user-visible because a refusal is a contract: the tool must not
// look like it returned a complete vocabulary when it stopped early.
func modelValuesNote() string {
	return valuesNote + fmt.Sprintf(" Model enumeration reads at most %d distinct raw spellings in at most %d page queries "+
		"and folds them into at most %d canonical names (%d bytes); past a bound it fails explicitly instead of returning a partial vocabulary.",
		modelValuesMaxRawSpellings, modelValuesMaxPageQueries, modelValuesMaxCanonical, modelValuesMaxCanonicalBytes)
}
