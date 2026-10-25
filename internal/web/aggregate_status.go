package web

import (
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// ---------- status / time derivation (Go mirror of the removed client code) ----------

// StatusClasses are the explorer's status facet values, in the order the
// dashboard publishes them. statusClass below is the classifier that produces
// exactly this vocabulary, and the web package's TestStatusClassAndBuckets
// keeps the two tied; the MCP reference list (internal/mcp StatusClasses)
// mirrors it and is pinned to it by the web package's scope-mirror test.
// Listing a class here that statusClass cannot return would advertise a
// silently empty filter.
var StatusClasses = []string{"2xx", "cancel", "4xx", "5xx", "err"}

// statusClass classifies a finalized code into the StatusClasses vocabulary.
// The cancel class reads the canonical code from internal/metrics
// (StatusClientClosedRequest), so the classifier, the MCP guide pin and the
// IsError predicate all use the one owner.
func statusClass(s int) string {
	switch {
	case s >= 200 && s < 300:
		return "2xx"
	case s == metrics.StatusClientClosedRequest:
		return "cancel"
	case s >= 400 && s < 500:
		return "4xx"
	case s >= 500:
		return "5xx"
	default:
		return "err"
	}
}

// contribStatusClass is the Go mirror of the client's statusClass(r): finalized
// codes use statusClass(int); a live row is paused / throttled / streaming / pending.
// Never treat a stored Stream flag or status 0 as "currently streaming".
func contribStatusClass(c *contrib) string {
	if c != nil && c.live {
		if c.paused {
			return "paused"
		}
		if c.throttled {
			return "throttled"
		}
		if c.stream {
			return "streaming"
		}
		return "pending"
	}
	if c == nil {
		return "err"
	}
	return statusClass(c.status)
}
