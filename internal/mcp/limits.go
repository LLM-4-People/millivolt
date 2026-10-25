package mcp

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// Operator token length bounds, the single owner of the credential band: the
// proxy's boot check (cmd/proxy/operator.go loadOperatorToken) and this
// server's ValidateOperatorToken both enforce these, and the proxy's
// bearerToken reads OperatorTokenMaxLen as its presentation cap. A credential
// the proxy would refuse to arm with is not a credential this server can use,
// and a longer one could never be presented as a Bearer value.
const (
	OperatorTokenMinLen = 16
	OperatorTokenMaxLen = 512
)

// ProxyTokenEnv is the proxy's own operator-token variable. The proxy's boot
// gate reads exactly this variable; this server never reads it, but its
// credential messages name it so a user knows where the value comes from.
const ProxyTokenEnv = "MILLIVOLT_OPERATOR_TOKEN"

// Limits are this server's result-size policy. They exist because a model
// cannot read ten thousand rows: every listing tool defaults to a small page
// and every payload is truncated with an explicit marker rather than silently.
// In production the values come from the boot config's mcp category through
// LimitsFromConfig; internal/config Default() owns the defaults and Validate
// below enforces internal/config's band constants, so this package carries no
// second copy of either.
type Limits struct {
	// QueryMaxRows is the default client-side row limit for a `query` call that
	// omits max_rows. An explicit max_rows may raise it, up to
	// config.MCPQueryMaxRowsMax. The proxy enforces its own
	// storage_query_max_rows (and answers 413 past it), which can be much
	// larger; this is the volume a model can actually read.
	QueryMaxRows int
	// QueryMaxBytes bounds the ENCODED size of one `query` result client-side.
	// The row cap alone is not a volume bound: a 200-row SELECT * of the
	// requests table measures well over 100k tokens in one result, which a
	// model cannot read and which is spent on columns it did not ask for. The
	// clamp keeps whole rows, so a truncated result is still valid JSON.
	QueryMaxBytes int
	// PageSize is the default page for records, values and audit capture
	// listings.
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

// LimitsFromConfig builds the endpoint's limits policy from the boot config's
// mcp category. It is the single owner of which config field feeds which
// limit; the values and their floors live in internal/config (Default and the
// band constants), so this only maps.
func LimitsFromConfig(c *config.Config) Limits {
	return Limits{
		QueryMaxRows:  c.MCPQueryMaxRows,
		QueryMaxBytes: int(c.MCPQueryMaxBytes),
		PageSize:      c.MCPPageSize,
		CaptureBytes:  int(c.MCPCaptureMaxBytes),
		QueryTimeout:  c.MCPQueryTimeout,
		Timeout:       c.MCPTimeout,
	}
}

// Validate rejects a limits set that could not page or could hang. The bands
// are internal/config's constants, the same single owner config.Validate
// enforces on the mcp_* fields, so the config boundary and this constructor
// guard cannot drift apart.
func (l Limits) Validate() error {
	switch {
	case l.QueryMaxRows < config.MCPQueryMaxRowsMin || l.QueryMaxRows > config.MCPQueryMaxRowsMax:
		return fmt.Errorf("query max rows must be %d..%d, got %d",
			config.MCPQueryMaxRowsMin, config.MCPQueryMaxRowsMax, l.QueryMaxRows)
	case l.QueryMaxBytes < config.MCPQueryMaxBytesMin:
		return fmt.Errorf("query max bytes must be at least %d", config.MCPQueryMaxBytesMin)
	case l.PageSize < config.MCPPageSizeMin || l.PageSize > config.MCPPageSizeMax:
		return fmt.Errorf("page size must be %d..%d, got %d",
			config.MCPPageSizeMin, config.MCPPageSizeMax, l.PageSize)
	case l.CaptureBytes < config.MCPCaptureMaxBytesMin:
		return fmt.Errorf("capture bytes must be at least %d", config.MCPCaptureMaxBytesMin)
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
		return errors.New("operator token is required: pass the proxy's " + ProxyTokenEnv + " value")
	case len(token) < OperatorTokenMinLen:
		return errors.New("operator token must be at least " + strconv.Itoa(OperatorTokenMinLen) + " bytes")
	case len(token) > OperatorTokenMaxLen:
		return errors.New("operator token must be at most " + strconv.Itoa(OperatorTokenMaxLen) + " bytes")
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

// resolveLimit is the one policy for every caller-supplied limit in this
// package. Zero means "use the configured default"; a negative value is not a
// synonym for unset and is rejected; a positive value must stay inside the
// tool's inclusive band. max == 0 means the argument has no additional ceiling
// because the proxy already bounds that payload (snapshot records and capture
// bytes). Centralizing it keeps the negative policy from drifting per handler.
func resolveLimit(field string, value, def, min, max int) (int, error) {
	if value < 0 {
		if max > 0 {
			return 0, fmt.Errorf("%s must be 0 (default) or %d..%d, got %d", field, min, max, value)
		}
		return 0, fmt.Errorf("%s must be 0 (default) or at least %d, got %d", field, min, value)
	}
	if value == 0 {
		return def, nil
	}
	if value < min || (max > 0 && value > max) {
		if max > 0 {
			return 0, fmt.Errorf("%s must be %d..%d, got %d", field, min, max, value)
		}
		return 0, fmt.Errorf("%s must be at least %d, got %d", field, min, value)
	}
	return value, nil
}

// clamp truncates a slice to limit and returns the marker describing what was
// withheld. marker names the shape being truncated ("rows", "records") and
// advice is the ONE way to continue for that shape, so the model acts on it
// instead of re-running the same oversized request. Advice is per call site on
// purpose: a generic "narrow the query or page with the cursor fields" is
// wrong for every tool that has no cursor fields, and actively sends a model
// looking for arguments its tool does not have.
func clamp[T any](items []T, limit int, marker, advice string) ([]T, Truncation) {
	return clampWithTotal(items, len(items), limit, marker, advice)
}

// clampWithTotal is clamp for a caller that counted the produced items without
// materializing them all. total is what the proxy produced; items holds only
// the prefix this server kept. The marker still names total, so a streamed
// result cut by the row cap cannot report the retained prefix as complete.
func clampWithTotal[T any](items []T, total, limit int, marker, advice string) ([]T, Truncation) {
	if len(items) > limit {
		items = items[:limit]
	}
	if total <= limit {
		return list(items), Truncation{Shown: len(items), Total: total}
	}
	return items, Truncation{
		Shown:     len(items),
		Total:     total,
		Truncated: true,
		Marker: fmt.Sprintf("truncated: %d of %d %s returned; %s",
			len(items), total, marker, advice),
	}
}

// rowsTruncatedByBytes is the marker for a query result whose encoded-size
// budget withheld rows. The streaming reader owns the counting and the
// whole-row rule; this owns the wording, so the two query clamps report their
// withholding in one place. total is the proxy's row count, not the row-capped
// intermediate, so a result cut by both limits cannot read as complete.
func rowsTruncatedByBytes(shown, total, used, budget int, advice string) Truncation {
	return Truncation{
		Shown:     shown,
		Total:     total,
		Truncated: true,
		Marker: fmt.Sprintf("truncated: %d of %d rows returned, about %d of the %d byte result budget; %s",
			shown, total, used, budget, advice),
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
