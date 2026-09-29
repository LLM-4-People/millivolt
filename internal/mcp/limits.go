package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Token length bounds. They mirror the proxy's own boot-time band
// (cmd/proxy/operator.go operatorTokenMinLen/MaxLen): a credential the proxy
// would refuse to arm with is not a credential this server can use, and a
// longer one could never be presented as a Bearer value.
const (
	operatorTokenMinLen = 16
	operatorTokenMaxLen = 512
)

// Limits are this server's result-size policy. They exist because a model
// cannot read ten thousand rows: every listing tool defaults to a small page
// and every payload is truncated with an explicit marker rather than silently.
type Limits struct {
	// QueryMaxRows bounds a `query` result client-side. The proxy enforces its
	// own storage_query_max_rows (and answers 413 past it), which can be much
	// larger; this is the volume a model can actually read.
	QueryMaxRows int
	// QueryMaxBytes bounds the ENCODED size of one `query` result client-side.
	// The row cap alone is not a volume bound: a 200-row SELECT * of the
	// requests table measures well over 100k tokens in one result, which a
	// model cannot read and which is spent on columns it did not ask for. The
	// clamp keeps whole rows, so a truncated result is still valid JSON.
	QueryMaxBytes int
	// PageSize is the default page for records and audit capture listings.
	PageSize int
	// CaptureBytes is the size above which a stored capture document is
	// withheld whole instead of being returned. A capture carries full request
	// and response bodies; a document that large is more than a model should
	// read in one call.
	CaptureBytes int
	// QueryTimeout bounds one proxy read that is not the server's own bounded
	// SQL reader (a full-history chart or explorer fold can take seconds).
	QueryTimeout time.Duration
	// Timeout bounds every HTTP call to the proxy, including mutations.
	Timeout time.Duration
}

// DefaultLimits are the built-in defaults for the flag/env surface in
// cmd/mcp. They exist only here; no consumer carries a fallback default.
func DefaultLimits() Limits {
	return Limits{
		QueryMaxRows:  200,
		QueryMaxBytes: 128 << 10,
		PageSize:      50,
		CaptureBytes:  256 << 10,
		QueryTimeout:  120 * time.Second,
		Timeout:       30 * time.Second,
	}
}

// Validate rejects a limits set that could not page or could hang.
func (l Limits) Validate() error {
	switch {
	case l.QueryMaxRows < 1:
		return errors.New("query max rows must be at least 1")
	case l.QueryMaxBytes < 1:
		return errors.New("query max bytes must be at least 1")
	case l.PageSize < 1:
		return errors.New("page size must be at least 1")
	case l.CaptureBytes < 1:
		return errors.New("capture bytes must be at least 1")
	case l.QueryTimeout <= 0:
		return errors.New("query timeout must be greater than zero")
	case l.Timeout <= 0:
		return errors.New("request timeout must be greater than zero")
	}
	return nil
}

// ValidateOperatorToken applies the proxy's length band. It is the only place
// the credential is inspected; the value itself is never returned, logged, or
// copied into any tool result.
func ValidateOperatorToken(token string) error {
	switch {
	case token == "":
		return errors.New("operator token is required: pass --operator-token or MILLIVOLT_MCP_OPERATOR_TOKEN (the proxy's MILLIVOLT_OPERATOR_TOKEN)")
	case len(token) < operatorTokenMinLen:
		return errors.New("operator token must be at least " + strconv.Itoa(operatorTokenMinLen) + " characters")
	case len(token) > operatorTokenMaxLen:
		return errors.New("operator token must be at most " + strconv.Itoa(operatorTokenMaxLen) + " characters")
	}
	return nil
}

// Truncation is the one owner of the explicit-marker convention shared by
// every tool that can return more rows than a model should read. Silently
// dropping rows would make a partial answer look like a complete one.
type Truncation struct {
	// Shown is how many items the tool returned.
	Shown int `json:"shown"`
	// Total is how many items the proxy produced before the cap.
	Total int `json:"total"`
	// Truncated is true when Total exceeded Shown.
	Truncated bool `json:"truncated"`
	// Marker is the human-legible explanation carried in the tool text.
	Marker string `json:"marker,omitempty"`
}

// cursorAdvice names the one continuation the two paired-cursor listings
// share: records and audit_captures_list both RETURN next_before_ms and
// next_before_id and take that pair back as before_ms and before_id. One
// owner, because a model that pages one listing pages the other the same way.
const cursorAdvice = "page with the next_before_ms and next_before_id pair this tool returned"

// clamp truncates a slice to limit and returns the marker describing what was
// withheld. marker names the shape being truncated ("rows", "records") and
// advice is the ONE way to continue for that shape, so the model acts on it
// instead of re-running the same oversized request. Advice is per call site on
// purpose: a generic "narrow the query or page with the cursor fields" is
// wrong for every tool that has no cursor fields, and actively sends a model
// looking for arguments its tool does not have.
func clamp[T any](items []T, limit int, marker, advice string) ([]T, Truncation) {
	if len(items) <= limit {
		return list(items), Truncation{Shown: len(items), Total: len(items)}
	}
	return items[:limit], Truncation{
		Shown:     limit,
		Total:     len(items),
		Truncated: true,
		Marker: fmt.Sprintf("truncated: %d of %d %s returned; %s",
			limit, len(items), marker, advice),
	}
}

// clampRowsToBytes keeps whole rows while their encoded size fits the budget.
// It never cuts inside a row, so a clamped result is still valid JSON a model
// can act on; the withheld rows are reported with the same explicit marker the
// row clamp uses, because a silently shortened result would read as a complete
// one.
//
// rows is the already row-capped slice and total is how many rows the proxy
// produced before that cap, so when BOTH limits fire the reported total is the
// proxy's, not the smaller intermediate. Understating it would tell a model a
// result was complete when it was not.
func clampRowsToBytes(rows []map[string]any, total, budget int, advice string) ([]map[string]any, Truncation) {
	kept := make([]map[string]any, 0, len(rows))
	used := 0
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			// A row the encoder cannot produce is not measurable, so it cannot
			// be budgeted; keep it rather than silently dropping data.
			kept = append(kept, row)
			continue
		}
		if used+len(encoded) > budget {
			break
		}
		used += len(encoded)
		kept = append(kept, row)
	}
	if len(kept) == len(rows) {
		return list(rows), Truncation{Shown: len(kept), Total: total}
	}
	return kept, Truncation{
		Shown:     len(kept),
		Total:     total,
		Truncated: true,
		Marker: fmt.Sprintf("truncated: %d of %d rows returned, about %d of the %d byte result budget; %s",
			len(kept), total, used, budget, advice),
	}
}

// list and object normalize an absent collection to an empty one. Every tool
// output is validated against its inferred JSON Schema, where a null is not an
// object or an array: an absent scope must read as empty, not as null.
func list[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

func object[T any](document map[string]T) map[string]T {
	if document == nil {
		return map[string]T{}
	}
	return document
}
