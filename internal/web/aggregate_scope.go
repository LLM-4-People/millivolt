package web

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// ---------- request scoping (chart, explorer, footer, log) ----------

// scopeFilter is one explorer filter (dim:value) - the same dims the client's
// request log is scoped by; the count endpoint matches them server-side over
// ALL history so "scope X of Y" is a since-inception number on both sides.
type scopeFilter struct {
	dim, id string
	// bit is the dimension's bit in the scope grouping mask, derived from the
	// canonical dimensionNames order at parse time. Zero means a directly
	// constructed fixture; scopedMatch computes it then.
	bit uint32
	// ids is the query-bound projection dictionary membership: the exact
	// label slot for client/provider/conversation/key, or every raw model
	// slot whose canonical spelling equals id. A non-nil empty slice means
	// the projection has no matching label (projected rows cannot match);
	// nil leaves the filter unbound for live-only matching.
	ids []uint32
}

// matches reports whether one contribution satisfies the filter; the Go
// mirror of the client's recordMatchesDim (same derivations: statusClass,
// timeBucket, multi-valued tools, error signatures from errorEntries).
func (f scopeFilter) matches(c *contrib) bool {
	switch f.dim {
	case "error":
		for _, e := range c.ent {
			if errorKey(e) == f.id {
				return true
			}
		}
		return false
	case "status":
		return contribStatusClass(c) == f.id
	case "time":
		return metrics.TimeBucket(time.UnixMilli(c.start)) == f.id
	case "tool":
		for _, t := range c.toolsL {
			if t == "" {
				continue // same empty-name skip as the explorer fold below
			}
			if t == f.id {
				return true
			}
		}
		return false
	case "client", "provider", "model", "conversation", "key":
		if c.rowIndex > 0 {
			dim, _ := singleValuedDim(f.dim)
			id := c.dimensionIDs[dim]
			if len(f.ids) == 1 {
				return id == f.ids[0]
			}
			// Model membership is appended in raw-dictionary order, so the
			// multi-spelling canonical case stays O(log n) per row.
			_, found := slices.BinarySearch(f.ids, id)
			return found
		}
		return c.liveLabel(f.dim) == f.id
	}
	return false
}

func parseScopeFilters(q url.Values) ([]scopeFilter, bool) {
	vals, ok := q["f"]
	if !ok {
		return nil, true
	}
	if len(vals) > 64 {
		return nil, false
	}
	out := make([]scopeFilter, 0, len(vals))
	for _, v := range vals {
		if v == "" || len(v) > 2048 {
			return nil, false
		}
		i := strings.IndexByte(v, ':')
		if i <= 0 {
			return nil, false
		}
		dim, id := v[:i], v[i+1:]
		if !validDims[dim] {
			return nil, false
		}
		out = append(out, scopeFilter{dim: dim, id: id, bit: scopeDimBit(dim)})
	}
	return out, true
}

// liveStatusClasses are the named in-flight pills the Requests-card status
// dropdown can send as s= (streaming / paused / throttled / pending). Distinct
// from explorer status CLASSES (2xx/4xx/…) which travel as f=status:2xx. The
// slice is the owner the MCP scope mirror is checked against.
var liveStatusClasses = []string{"paused", "throttled", "streaming", "pending"}

func liveStatusFilter(s string) bool {
	return slices.Contains(liveStatusClasses, s)
}

// parseScope parses a scoping request: explorer filters (f=dim:id, repeated -
// same dims the log is scoped by) plus the Requests-card status dropdown
// (s=200: an exact HTTP code, OR s=streaming/paused/…: a live in-flight
// class). Unknown s= is rejected (deny by default) - never silently ignored.
// q is the handler's one strict-parsed query (adminjson.StrictQuery); the
// repeated f= list key keeps its multi-value grammar.
func parseScope(q url.Values) (fs []scopeFilter, statusCode string, ok bool) {
	fs, ok = parseScopeFilters(q)
	if !ok {
		return nil, "", false
	}
	if s := q.Get("s"); s != "" {
		var valid bool
		statusCode, valid = canonicalStatusSelector(s)
		if !valid {
			return nil, "", false
		}
	}
	return fs, statusCode, true
}

// canonicalStatusSelector is the one status-spelling owner for every web
// surface: a live in-flight class stays verbatim, and an exact numeric code is
// rendered in canonical decimal form. "0200" and "+200" therefore match the
// same rows as "200" in chart, explorer and log instead of being accepted by
// validation and silently matching zero in the byte-for-byte Go matcher.
func canonicalStatusSelector(s string) (string, bool) {
	if liveStatusFilter(s) {
		return s, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 9999 {
		return "", false
	}
	return strconv.Itoa(n), true
}

// scopedMatch reports whether a contribution satisfies the full log scope
// (Requests-card status filter + explorer filters). Filters are AND-ed across
// dimensions and OR-ed within one dimension: repeated f=model:a&f=model:b are
// alternatives, exactly the grammar the MCP scope and the public docs state.
// The first filter of each dimension evaluates the whole group, later members
// are skipped, so each group matches at most once.
func scopedMatch(c *contrib, statusCode string, fs []scopeFilter) bool {
	if statusCode != "" {
		if liveStatusFilter(statusCode) {
			if contribStatusClass(c) != statusCode {
				return false
			}
		} else if !statusCodeEquals(c.status, statusCode) {
			return false
		}
	}
	// One pass with two dimension masks preserves OR-within and AND-across:
	// seen records every dimension that must match, matched records the
	// dimensions already satisfied. After one member of a dimension matches,
	// later members are skipped without rescanning the remaining slice.
	var seen, matched uint32
	for i := range fs {
		bit := fs[i].bit
		if bit == 0 {
			bit = scopeDimBit(fs[i].dim)
		}
		if bit == 0 {
			return false
		}
		seen |= bit
		if matched&bit != 0 {
			continue
		}
		if fs[i].matches(c) {
			matched |= bit
		}
	}
	return matched == seen
}

// statusCodeEquals compares a contribution's numeric status with the validated
// exact-code scope without formatting the number into a new string per row
// (strconv.Itoa allocated once per match attempt on every fold).
func statusCodeEquals(code int, text string) bool {
	if code < 0 {
		return false
	}
	var buf [20]byte
	i := len(buf)
	n := code
	for {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			break
		}
	}
	digits := buf[i:]
	if len(digits) != len(text) {
		return false
	}
	for j := range digits {
		if digits[j] != text[j] {
			return false
		}
	}
	return true
}
