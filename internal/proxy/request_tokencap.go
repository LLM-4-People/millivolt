package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/jsonnum"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// resolveRequestTokenCap owns the three wire spellings the metadata decoder
// accepts: the Chat pair (max_completion_tokens over max_tokens) first, then the
// Responses max_output_tokens spelling. Every metadata decode path calls it, so
// the hostile-cap trust boundary cannot see a different ceiling per path.
func resolveRequestTokenCap(maxCompletion, maxTokens, maxOutput *int) *int {
	if v := providerformat.MergeTokenCaps(maxCompletion, maxTokens); v != nil {
		return v
	}
	return maxOutput
}

// applyTokenCaps stamps the effective token ceiling onto the record through
// MergeTokenCaps' precedence (max_completion_tokens over max_tokens over
// max_output_tokens; none present stamps nil). The occurrence scan rejects a
// duplicate cap spelling, and a malformed spelling leaves the ceiling unset
// here; ServeHTTP's tokenCapViolation rejects either before the request can
// proceed. A caller that already holds a scan applies it directly, so the
// preflighted and rewritten bodies are scanned once each.
func applyTokenCaps(body []byte, rec *metrics.Record) {
	scanTokenCaps(body).apply(rec)
}

// apply stamps the effective token ceiling from an already computed occurrence
// scan, so a preflighted body is not scanned a second time. A rewritten body
// restamps from its own scan, so the hostile-cap boundary, the scheduler
// reservation and the recorded request parameters all describe the bytes
// actually sent upstream; a caps decode that still fails (the client's own cap
// field carried a non-number) keeps the first decode's value rather than
// fabricating one.
func (s tokenCapScan) apply(rec *metrics.Record) {
	if s.duplicate != "" {
		return
	}
	rec.ReqMaxTokens = resolveRequestTokenCap(
		tokenCapOptional(s.caps.MaxCompTokens, "max_completion_tokens"),
		tokenCapOptional(s.caps.MaxTokens, "max_tokens"),
		tokenCapOptional(s.caps.MaxOutputTokens, "max_output_tokens"),
	)
}

// tokenCapOptional decodes one cap member for the request record. A missing or
// malformed spelling contributes nothing here; ServeHTTP's tokenCapViolation is
// the trust boundary that rejects a malformed cap before the request proceeds.
func tokenCapOptional(raw json.RawMessage, field string) *int {
	v, err := tokenCapValue(raw, field)
	if err != nil {
		return nil
	}
	return v
}

// maxRequestOutputTokens bounds a client-supplied max_tokens /
// max_completion_tokens at the trust boundary. Mainstream model output caps
// top out around 128k tokens; anything beyond 1,000,000 (the same ceiling
// internal/config validates for anthropic_default_max_tokens) is hostile or
// malformed. Unbounded values would let one request wrap the Acquire-time
// token estimate negative or debit a provider token bucket by the full
// attacker-chosen amount.
const maxRequestOutputTokens = 1_000_000

// hostileTokenCap reports whether a decoded token cap exceeds the
// maxRequestOutputTokens bound. Both ServeHTTP checks share it: the check
// on the original body's decode and the check on the fresh record a
// translated body re-decodes into.
func hostileTokenCap(v *int) bool {
	return v != nil && *v > maxRequestOutputTokens
}

// tokenCapMembers holds the first raw occurrence of each accepted token-ceiling
// spelling. It is the occurrence scan's container, not an independently decoded
// record: encoding/json folds member names case-insensitively and keeps the
// last duplicate, while the relay sends the body byte-for-byte, so a second
// occurrence could describe a different value to the record and the wire.
type tokenCapMembers struct {
	MaxTokens       json.RawMessage `json:"max_tokens"`
	MaxCompTokens   json.RawMessage `json:"max_completion_tokens"`
	MaxOutputTokens json.RawMessage `json:"max_output_tokens"`
}

// tokenCapNames names one spellings-array slot in scan order, so the duplicate
// rejection and the value validation read one list.
var tokenCapNames = [3]string{"max_tokens", "max_completion_tokens", "max_output_tokens"}

// tokenCapScan is one occurrence scan of a body's token-ceiling spellings. seen
// tracks the spellings already recorded; duplicate names the first spelling
// that appeared twice, which drops every recorded value so the scan result is
// exactly the pre-rejection state.
type tokenCapScan struct {
	caps      tokenCapMembers
	seen      [3]bool
	duplicate string
}

// capture records one spelling occurrence. seen and caps are the scan state, so
// every occurrence scan - the standalone walk and the preflight's shared
// top-level walk - applies the duplicate rule identically.
func (s *tokenCapScan) capture(slot int, raw json.RawMessage) {
	if s.duplicate != "" {
		return
	}
	if s.seen[slot] {
		s.duplicate = tokenCapNames[slot]
		s.caps = tokenCapMembers{}
		return
	}
	s.seen[slot] = true
	switch slot {
	case 0:
		s.caps.MaxTokens = raw
	case 1:
		s.caps.MaxCompTokens = raw
	case 2:
		s.caps.MaxOutputTokens = raw
	}
}

// tokenCapSlot reports the spellings-array slot a top-level key names, or -1.
// encoding/json matches members case-insensitively, so the comparison folds.
func tokenCapSlot(key requestKey) int {
	for slot, name := range tokenCapNames {
		if key.equal(name) {
			return slot
		}
	}
	return -1
}

// tokenCapPresent reports whether a raw cap member was supplied. JSON null is
// the schema's explicit absent value.
func tokenCapPresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// scanTokenCaps walks the top-level object once and collects the first raw
// value of each accepted token-ceiling spelling. A second occurrence of any
// member that folds to a cap name is recorded instead of letting the record and
// reservation validate one value while the relay sends every occurrence
// upstream. Invalid JSON returns an empty scan so the existing passthrough
// path is preserved.
func scanTokenCaps(body []byte) tokenCapScan {
	var scan tokenCapScan
	i := metrics.SkipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return scan
	}
	i = metrics.SkipSpace(body, i+1)
	for i < len(body) && body[i] != '}' {
		keyStart := i
		end := skipJSONString(body, i)
		if end < 0 {
			return tokenCapScan{}
		}
		key, ok := decodeRequestKey(body[keyStart:end])
		if !ok {
			return tokenCapScan{}
		}
		i = metrics.SkipSpace(body, end)
		if i >= len(body) || body[i] != ':' {
			return tokenCapScan{}
		}
		i = metrics.SkipSpace(body, i+1)
		if i >= len(body) {
			return tokenCapScan{}
		}
		valueStart := i
		next := skipJSONValue(body, i)
		if next < 0 {
			return tokenCapScan{}
		}
		if slot := tokenCapSlot(key); slot >= 0 {
			scan.capture(slot, json.RawMessage(body[valueStart:next]))
		}
		i = metrics.SkipSpace(body, next)
		if i < len(body) && body[i] == ',' {
			i = metrics.SkipSpace(body, i+1)
			continue
		}
		if i < len(body) && body[i] == '}' {
			break
		}
		return tokenCapScan{}
	}
	return scan
}

// tokenCapValue validates one raw token-ceiling member. An absent or JSON null
// member returns nil with no error. A supplied member must be a JSON number
// whose value is integral and no greater than maxRequestOutputTokens; the
// returned int is that exact value. The wire schema requires an integer, so a
// fractional, overflowing, out-of-range or non-number value fails closed
// instead of slipping past a typed decode that rejected the whole object.
func tokenCapValue(raw json.RawMessage, field string) (*int, error) {
	if !tokenCapPresent(raw) {
		return nil, nil
	}
	n, err := jsonnum.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s %s", field, err)
	}
	if n > maxRequestOutputTokens {
		return nil, fmt.Errorf("%s must not exceed %d", field, maxRequestOutputTokens)
	}
	return &n, nil
}

// conflictingTokenCapSpellings reports a body that mixes the Responses
// max_output_tokens spelling with a Chat max_tokens/max_completion_tokens
// spelling. Presence is decided on the occurrence scan, so an unrepresentable
// spelling cannot hide the mix. The wire families are distinct, so accepting
// both would make the recorded ceiling depend on decode precedence rather than
// the client's wire.
func conflictingTokenCapSpellings(body []byte) bool {
	return scanTokenCaps(body).mixesSpellings()
}

// mixesSpellings reports a mixed-spelling scan result. A duplicate makes the
// scan unusable, matching the standalone helper's previous contract.
func (s tokenCapScan) mixesSpellings() bool {
	if s.duplicate != "" {
		return false
	}
	return tokenCapPresent(s.caps.MaxOutputTokens) &&
		(tokenCapPresent(s.caps.MaxTokens) || tokenCapPresent(s.caps.MaxCompTokens))
}

// tokenCapViolation returns the client-facing 400 reason for the first token
// ceiling spelling that is hostile, malformed or duplicated, or the empty
// string when every occurrence is valid. It checks every occurrence the body
// carries, not only the one that wins the record precedence: a Responses body
// can carry both max_tokens and max_output_tokens, and checking only the winner
// let the other spelling reach the upstream unbounded. The body is authoritative
// because it carries the client's actual field names; the record's effective
// ceiling is a fallback for a body whose cap fields did not decode.
func tokenCapViolation(body []byte, rec *metrics.Record) string {
	return scanTokenCaps(body).violation(rec)
}

// violation is tokenCapViolation over an already computed occurrence scan. The
// duplicate reason comes first because a duplicate drops the recorded values.
func (s tokenCapScan) violation(rec *metrics.Record) string {
	if s.duplicate != "" {
		return s.duplicate + " appears more than once"
	}
	for _, cap := range []struct {
		name string
		raw  json.RawMessage
	}{
		{tokenCapNames[1], s.caps.MaxCompTokens},
		{tokenCapNames[0], s.caps.MaxTokens},
		{tokenCapNames[2], s.caps.MaxOutputTokens},
	} {
		if _, err := tokenCapValue(cap.raw, cap.name); err != nil {
			return err.Error()
		}
	}
	if rec != nil && hostileTokenCap(rec.ReqMaxTokens) {
		return fmt.Sprintf("output token cap must not exceed %d", maxRequestOutputTokens)
	}
	return ""
}

// writeHostileTokenCap rejects a hostile or malformed token cap with the shared
// 400 envelope. Both ServeHTTP enforcement sites use it, so the two trust
// boundaries can never drift in status or type.
func writeHostileTokenCap(w http.ResponseWriter, inbound ir.Protocol, reason string) {
	writeProtocolHTTPError(w, inbound, http.StatusBadRequest, typeInvalidRequestError, reason)
}
