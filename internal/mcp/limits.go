package mcp

import (
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
	// PageSize is the default page for records and audit capture listings.
	PageSize int
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
		QueryMaxRows: 200,
		PageSize:     50,
		QueryTimeout: 120 * time.Second,
		Timeout:      30 * time.Second,
	}
}

// Validate rejects a limits set that could not page or could hang.
func (l Limits) Validate() error {
	switch {
	case l.QueryMaxRows < 1:
		return errors.New("query max rows must be at least 1")
	case l.PageSize < 1:
		return errors.New("page size must be at least 1")
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

// clamp truncates a slice to limit and returns the marker describing what was
// withheld. marker names the shape being truncated ("rows", "records") and the
// advice for continuing, so the model can act on it.
func clamp[T any](items []T, limit int, marker string) ([]T, Truncation) {
	if len(items) <= limit {
		return list(items), Truncation{Shown: len(items), Total: len(items)}
	}
	return items[:limit], Truncation{
		Shown:     limit,
		Total:     len(items),
		Truncated: true,
		Marker: fmt.Sprintf("truncated: %d of %d %s returned; narrow the query or page with the cursor fields",
			limit, len(items), marker),
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
