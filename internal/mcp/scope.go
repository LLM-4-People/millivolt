package mcp

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Scope is the one owner of the explorer's filter grammar, shared by the
// explore, chart and records tools. The proxy parses it; this encodes it once
// so no tool can spell `f=dim:id` differently, split on the wrong colon, or
// smuggle a second status selector.
//
// Grammar, as the proxy accepts it:
//   - `dim:id` repeated, at most 64, split on the FIRST colon. Filters are
//     AND-ed across dimensions and OR-ed within one dimension's values
//     (several `f=model:a`, `f=model:b` entries are alternatives).
//   - `s=` is a single selector: an exact HTTP status code (0..9999) or one of
//     the live in-flight classes. It is NOT the explorer's status class
//     vocabulary, which travels as `f=status:2xx`.
//
// The JSON names are the tool-facing argument names: a tool input embeds Scope,
// and encoding/json promotes an embedded struct's tagged fields, so every tool
// that accepts a scope names them identically.
type Scope struct {
	// Filters are raw `dim:id` strings as the caller wrote them.
	Filters []string `json:"filters,omitempty" jsonschema:"repeated dim:id filters, AND-ed across dimensions and OR-ed within one dimension"`
	// Status is the single s= selector, empty for none.
	Status string `json:"status,omitempty" jsonschema:"exact HTTP status code 0..9999, or a live class: streaming, paused, throttled, pending"`
}

// liveStatusClasses are the in-flight states the Requests-card status selector
// accepts alongside exact codes.
var liveStatusClasses = []string{"streaming", "paused", "throttled", "pending"}

// Dimensions are the explorer's facet dimensions. The proxy owns the canonical
// set; this list exists so a tool description can name them and so a bad value
// is rejected locally with a useful message instead of a bare 400.
var Dimensions = []string{"client", "provider", "model", "conversation", "key", "status", "time", "tool", "error"}

// maxScopeFilters mirrors the proxy's repeated-filter bound.
const maxScopeFilters = 64

// Validate applies the grammar the proxy enforces, so a malformed scope is a
// readable tool message rather than a round trip. Deny by default: an unknown
// dimension or status is rejected instead of being ignored.
func (s Scope) Validate() error {
	if len(s.Filters) > maxScopeFilters {
		return fmt.Errorf("at most %d filters are allowed, got %d", maxScopeFilters, len(s.Filters))
	}
	for _, filter := range s.Filters {
		if err := validateScopeFilter(filter); err != nil {
			return err
		}
	}
	return validateScopeStatus(s.Status)
}

func validateScopeFilter(filter string) error {
	dim, id, ok := strings.Cut(filter, ":")
	if !ok || dim == "" || id == "" {
		return fmt.Errorf("filter %q must be dim:id, for example provider:x.ai or status:2xx", filter)
	}
	for _, known := range Dimensions {
		if dim == known {
			return nil
		}
	}
	return fmt.Errorf("filter dimension %q is unknown; use one of %s", dim, strings.Join(Dimensions, ", "))
}

func validateScopeStatus(status string) error {
	if status == "" {
		return nil
	}
	for _, class := range liveStatusClasses {
		if status == class {
			return nil
		}
	}
	code, err := strconv.Atoi(status)
	if err != nil || code < 0 || code > 9999 {
		return fmt.Errorf("status %q must be an exact HTTP status code (0..9999) or one of %s",
			status, strings.Join(liveStatusClasses, ", "))
	}
	return nil
}

// query renders the scope into the proxy's query parameters. It is the only
// place f= and s= are produced.
func (s Scope) query() url.Values {
	values := url.Values{}
	for _, filter := range s.Filters {
		values.Add("f", filter)
	}
	if s.Status != "" {
		values.Set("s", s.Status)
	}
	return values
}

// merge copies base query values and overlays the scope's parameters, so a
// tool's own single-occurrence key (dim, window, limit) and the scope coexist
// in one deterministic encoding.
func (s Scope) merge(base url.Values) url.Values {
	values := url.Values{}
	for key, list := range base {
		for _, value := range list {
			values.Add(key, value)
		}
	}
	for key, list := range s.query() {
		values[key] = append(values[key], list...)
	}
	return values
}

// RecordsPage is one durable newest-first page from /metrics/agg/log. The
// proxy scans a bounded number of rows per call, so a page can be short or
// empty while `more` is still true; callers must treat a non-advancing cursor
// as exhaustion instead of looping.
type RecordsPage struct {
	Records  []map[string]any `json:"records"`
	More     bool             `json:"more"`
	CursorMs int64            `json:"cursor_ms"`
	CursorID string           `json:"cursor_id"`
}

// recordsPage fetches one page. beforeMs/beforeID form the paired keyset
// cursor; both or neither, matching the proxy's rule. limit must already be
// inside the proxy's page band.
func (c *Client) recordsPage(ctx context.Context, query url.Values, beforeMs int64, beforeID string, limit int) (*RecordsPage, error) {
	values := url.Values{}
	for key, list := range query {
		for _, value := range list {
			values.Add(key, value)
		}
	}
	values.Set("limit", strconv.Itoa(limit))
	if beforeID != "" {
		values.Set("before_ms", strconv.FormatInt(beforeMs, 10))
		values.Set("before_id", beforeID)
	}
	var page RecordsPage
	if err := c.getJSON(ctx, "/metrics/agg/log", values, &page); err != nil {
		return nil, err
	}
	return &page, nil
}
