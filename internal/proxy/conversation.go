package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// maxDeclaredSessionBytes bounds retained relationship identifiers. It applies
// to the opt-in parent declaration and to a session-only header; both are
// client claims that become record fields.
const maxDeclaredSessionBytes = 512

// validDeclaredSession is the single owner of the retained-identifier bound:
// non-empty, at most maxDeclaredSessionBytes, valid UTF-8, no control bytes.
// requestConversation rejects a violating header declaration (parent or
// session-only) at the request boundary; the sub-conversation extraction drops
// a violating tracked value (never a 400 - the body is passthrough payload).
func validDeclaredSession(id string) bool {
	return id != "" && len(id) <= maxDeclaredSessionBytes && utf8.ValidString(id) &&
		!strings.ContainsFunc(id, func(r rune) bool { return r < ' ' || r == 0x7f })
}

// invalidDeclaredSessionError is the one wording for both request-boundary
// rejections (session-only and parent declaration).
func invalidDeclaredSessionError() error {
	return fmt.Errorf("declared session identifiers must be nonempty printable text of at most %d bytes", maxDeclaredSessionBytes)
}

// requestConversation owns the optional parent declaration boundary. A parent
// requires the child's explicit identity. A session-only header shares the
// same identifier rules, while an absent or blank session keeps the existing
// automatic-grouping semantics. These identifiers are client claims, not proof
// of agent spawning, and must never be inferred from messages or response IDs.
func requestConversation(h http.Header) (session, parent string, err error) {
	session = strings.TrimSpace(h.Get(hdrSession))
	parents, declared := h[http.CanonicalHeaderKey(hdrParentSession)]
	if !declared {
		if session != "" && !validDeclaredSession(session) {
			return "", "", invalidDeclaredSessionError()
		}
		return session, "", nil
	}
	if len(parents) != 1 || len(h.Values(hdrSession)) != 1 {
		return "", "", fmt.Errorf("%s requires exactly one %s and one parent value", hdrParentSession, hdrSession)
	}
	parent = strings.TrimSpace(parents[0])
	for _, id := range []string{session, parent} {
		if !validDeclaredSession(id) {
			return "", "", invalidDeclaredSessionError()
		}
	}
	if session == parent {
		return "", "", fmt.Errorf("a session cannot declare itself as its parent")
	}
	return session, parent, nil
}

// explicitConversationID namespaces one declared session. A valid id keeps the
// original byte-identical s:<id> form; a direct Assign with an invalid or
// overlong id hashes the whole value so distinct inputs cannot collide through
// truncation.
func explicitConversationID(id string) string { return namespacedConversationID("s:", id) }

// trackedConversationID namespaces a sub-conversation param identity beside
// explicitConversationID's s: sessions: a k: id can never collide with an
// explicit session or a c- auto grouping.
func trackedConversationID(id string) string { return namespacedConversationID("k:", id) }

// namespacedConversationID is the single defensive owner for both identifier
// namespaces. The hashed form is bounded at 68 bytes (prefix + h: + sha256
// hex); an empty id also hashes rather than producing a bare namespace.
func namespacedConversationID(prefix, id string) string {
	if validDeclaredSession(id) {
		return prefix + id
	}
	sum := sha256.Sum256([]byte(id))
	return prefix + "h:" + hex.EncodeToString(sum[:])
}

// subConversationScanMax bounds the raw byte scan of one tracked param's
// string value. The bound is judged on raw bytes, before the decoded value is
// trimmed: a value with no trimmable edges that still satisfies
// maxDeclaredSessionBytes never spans more raw bytes than this - every JSON
// escape sequence spends at least two raw bytes per decoded byte (one
// six-byte \uXXXX escape per decoded byte is the worst case), plus the two
// framing quotes. The window only bounds how far the scan looks for the
// closing quote: a present string whose raw span exceeds it is the same
// present-dropped outcome as a value that fails the declared-session bound
// (never a 400 - the body is passthrough payload). The drop is deliberately
// conservative: a value whose trimmable edges push its raw span past the
// window is dropped even though its trimmed form would pass the identity
// bound, so the scan cap can narrow user-visible semantics in exactly this
// one direction - never silently broaden one.
// Internal safety guardrail, not user-tunable.
const subConversationScanMax = 6*maxDeclaredSessionBytes + 2

// resolveSubConversation is the sub_conversations request-path owner: it
// resolves the classified client's configured entry and extracts the tracked
// value from the already-buffered request body. No entries configured, or no
// exact client leaf match, returns (nil, "") - the feature-off path leaves the
// request byte-identical and untracked. For a matched entry the configured
// params are checked in order and the first present one decides: a value that
// is not a string counts as absent (the next param is checked), while a
// present string that cannot be decoded or fails the identity rules drops
// the identity for the request (which stays on automatic grouping),
// never a 400 - the body is passthrough payload, the configToken deny-by-drop
// precedent.
// No second full-body parse runs: the locate is a depth-aware lexical scan of
// the buffered bytes, with the shared negative pre-gate keeping param-free
// regions on the vectorized path.
func resolveSubConversation(entries []config.SubConversation, client string, body []byte) (entry *config.SubConversation, value string) {
	if len(entries) == 0 {
		return nil, ""
	}
	for i := range entries {
		if entries[i].Client == client {
			entry = &entries[i]
			break
		}
	}
	if entry == nil {
		return nil, ""
	}
	// One backslash probe, computed only when a param's literal spelling is
	// absent: the common satisfied case never pays the full-body escape scan.
	for _, name := range entry.Params {
		if v, present := subConversationParam(body, name); present {
			return entry, v
		}
	}
	return entry, ""
}

// subConversationParam locates one configured param's string value among the
// buffered body's TOP-LEVEL object keys - a request body param is top-level
// by product semantics, so an occurrence of the same spelling nested inside
// a value is neither tracked nor stripped. present reports that the param
// decided the identity: a present value that is not a string counts as
// absent (the next param is checked), while a present string that cannot
// be decoded or fails the identity rules drops the identity for the
// request - the value comes back empty with present true, never a
// fall-through to a later param, so the configured order stays
// presence-ordered, never value-quality-ordered. More than one top-level
// occurrence of the same param is ambiguous and drops the identity the same
// way: present, empty, automatic grouping.
func subConversationParam(body []byte, name string) (value string, present bool) {
	// Keep param-free regions on the vectorized negative path (the
	// HasErrorKey pre-gate class): a body without the literal quoted spelling
	// and without any escape sequence cannot carry the key. The param grammar
	// forbids quote, backslash and control bytes in the name itself, so a key
	// can only reach the wire spelled literally or through escape sequences.
	if !containsQuotedName(body, name) && !bytes.ContainsRune(body, '\\') {
		return "", false
	}
	tail, duplicate := topLevelJSONKey(body, name)
	if len(tail) == 0 {
		return "", false
	}
	if duplicate {
		// Two top-level occurrences are ambiguous: JSON consumers take the
		// last value while a first-match scan sees the earlier one. Deny by
		// default for identity - present with no value drops the tracked
		// identity and leaves the request on automatic grouping rather than
		// guessing which occurrence the upstream will honor.
		return "", true
	}
	if tail[0] != '"' {
		return "", false
	}
	window := tail
	if len(window) > subConversationScanMax {
		window = window[:subConversationScanMax]
	}
	end := -1
	for i := 1; i < len(window); i++ {
		if window[i] == '"' {
			end = i
			break
		}
		if window[i] == '\\' {
			i++
		}
	}
	if end < 0 {
		// Unterminated within the scan window: the raw span either exceeds
		// it (a value too long for any bounded value, or one whose trimmable
		// edges alone push the span past the window even though its trimmed
		// form would pass) or the body ends before the closing quote (a
		// truncated body) - both are the same present-dropped class as the
		// identity rules.
		return "", true
	}
	token := window[:end+1]
	// The standard decoder coerces invalid UTF-8 inside strings to U+FFFD;
	// the identity bound must judge the client's actual bytes, so validate
	// before decoding.
	if !utf8.Valid(token[1:end]) {
		return "", true
	}
	var s string
	if json.Unmarshal(token, &s) != nil {
		return "", true
	}
	s = strings.TrimSpace(s)
	if !validDeclaredSession(s) {
		return "", true
	}
	return s, true
}

// containsQuotedName reports whether body contains the literal quoted spelling
// of a top-level key. The stack buffer avoids a per-param allocation for the
// normal short parameter name.
func containsQuotedName(body []byte, name string) bool {
	var buf [64]byte
	if len(name)+2 <= len(buf) {
		quoted := append(buf[:0], '"')
		quoted = append(quoted, name...)
		quoted = append(quoted, '"')
		return bytes.Contains(body, quoted)
	}
	return bytes.Contains(body, []byte(`"`+name+`"`))
}

// topLevelJSONKey locates the named key among the members of the body's
// top-level JSON object and returns the input tail beginning at its first
// value; nil when the body is not a JSON object or the key is not a top-level
// member. duplicate reports a second top-level occurrence of the same key, the
// ambiguous shape identity extraction refuses to guess at. This is the
// depth-aware sibling of metrics.JSONKey's any-depth walk (which stays with the
// SSE and error-class consumers): keys inside nested objects or arrays never
// match. A lexical scan of the buffered bytes, never a second document decode -
// string escapes are honored, and a body too malformed to finish the scan
// returns the first match it already found (or nothing before one).
func topLevelJSONKey(data []byte, key string) (tail []byte, duplicate bool) {
	i := metrics.SkipSpace(data, 0)
	if i >= len(data) || data[i] != '{' {
		return nil, false
	}
	i = metrics.SkipSpace(data, i+1)
	for {
		if i >= len(data) || data[i] == '}' {
			return tail, duplicate
		}
		if data[i] != '"' {
			return tail, duplicate // a member must open with a key string
		}
		start := i
		i++
		escaped := false
		for i < len(data) && data[i] != '"' {
			if data[i] == '\\' {
				escaped = true
				i++
			}
			i++
		}
		if i >= len(data) {
			return tail, duplicate
		}
		end := i // the key's closing quote
		i = metrics.SkipSpace(data, i+1)
		if i >= len(data) || data[i] != ':' {
			return tail, duplicate
		}
		i = metrics.SkipSpace(data, i+1)
		if i >= len(data) {
			return tail, duplicate
		}
		match := string(data[start+1:end]) == key
		if escaped {
			var name string
			match = json.Unmarshal(data[start:end+1], &name) == nil && name == key
		}
		if match {
			if tail != nil {
				return tail, true
			}
			tail = data[i:]
		}
		i = skipJSONValue(data, i)
		if i < 0 {
			return tail, duplicate
		}
		i = metrics.SkipSpace(data, i)
		if i >= len(data) || data[i] != ',' {
			return tail, duplicate // not a member separator: the scan is over
		}
		i = metrics.SkipSpace(data, i+1)
	}
}

// skipJSONValue and skipJSONString are the proxy-side names for the shared
// lexical scanner in metrics. One implementation owns JSON value/string
// traversal for the metadata preflight and the content-part bound.
func skipJSONValue(data []byte, i int) int { return metrics.SkipJSONValue(data, i) }

func skipJSONString(data []byte, i int) int { return metrics.SkipJSONString(data, i) }

// conversation.go reconstructs LLM "conversations" (multi-request tasks) from
// stateless requests, without any client cooperation or hardcoded client rules.
//
// Clients that send full conversation history make the request's turn count
// grow monotonically within one conversation and reset when a new one starts.
// We exploit that: within a (client, api-key) partition, a request continues an
// open conversation when its turn count is >= the conversation's last-seen
// count, and starts a new one on a reset (count drops) or after an idle gap.
//
// Concurrent conversations from the same client are disambiguated by keeping
// several open conversations per partition and matching each request to the
// open conversation whose last turn count is the greatest value still <= the
// request's count (nearest-prefix rule) - parallel conversations grow their own
// independent counts, so each request lands on the one it extends.

// convoSession is one open conversation being tracked.
type convoSession struct {
	id       string
	lastTurn int       // highest total turn count seen so far
	lastAt   time.Time // last request time (for idle-gap expiry)
}

// convoPartition holds the open conversations for one (client, key) pair.
type convoPartition struct {
	// open sessions in creation order (oldest first); there is no move-to-front
	// on match, so eviction (newSession) drops the oldest-CREATED session.
	sessions []*convoSession
}

// ConversationTracker assigns a stable conversation id to each request. It is
// safe for concurrent use and bounds memory by expiring idle sessions lazily.
// conversationPartitionsMax bounds distinct client+key partitions retained in
// memory. A hostile client rotating keys/labels can otherwise grow the map for
// the process lifetime; at the cap the oldest-created partition is evicted.
const conversationPartitionsMax = 65_536

type ConversationTracker struct {
	mu    sync.Mutex
	parts map[string]*convoPartition
	// order tracks live partitions for O(1) eviction at the cap. An idle sweep
	// rebuilds it from the survivors, so eviction order after a sweep is not
	// creation order; the cap is a memory bound, not a fairness contract.
	order     []string
	sequence  uint64 // process-lifetime sequence; no expired partition keys retained
	idleGap   time.Duration
	maxOpen   int       // per-partition cap on concurrently-open conversations
	lastSweep time.Time // last global idle-partition sweep (throttled to idleGap)
}

// UpdateLimits hot-applies new idleGap/maxOpen on a live tracker (config
// reload). Existing sessions are unaffected; the new limits govern subsequent
// matching, eviction, and sweeps.
func (t *ConversationTracker) UpdateLimits(idleGap time.Duration, maxOpen int) {
	if maxOpen < 1 {
		maxOpen = 1
	}
	t.mu.Lock()
	t.idleGap = idleGap
	t.maxOpen = maxOpen
	t.mu.Unlock()
}

// NewConversationTracker creates a tracker. idleGap is the inactivity window
// after which a request is treated as a new conversation; maxOpen caps
// concurrently-open conversations per partition (oldest evicted past it).
func NewConversationTracker(idleGap time.Duration, maxOpen int) *ConversationTracker {
	// Defensive floor: maxOpen < 1 would make the eviction slice expression in
	// newSession panic. The real default lives in config.Default(); this only
	// guards against a misconstructed tracker.
	if maxOpen < 1 {
		maxOpen = 1
	}
	return &ConversationTracker{
		parts:   make(map[string]*convoPartition),
		idleGap: idleGap,
		maxOpen: maxOpen,
	}
}

// convoKey builds the partition key. The key hash scopes it to one credential
// so two users on the same client never share a conversation.
func convoKey(client, keyHash string) string {
	return client + "|" + keyHash
}

// Assign returns the conversation id for a request. explicitID (from
// X-Proxy-Session) wins when present, then trackedID (a sub_conversations
// body-param value); each is namespaced by its own owner so neither identity
// can collide with the other or with an auto id. Otherwise the request is
// auto-grouped by turn count. totalTurns is turns_user+turns_assistant+turns_tool.
func (t *ConversationTracker) Assign(client, keyHash, explicitID, trackedID string, totalTurns int, now time.Time) string {
	if explicitID != "" {
		return explicitConversationID(explicitID)
	}
	if trackedID != "" {
		return trackedConversationID(trackedID)
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	// Opportunistically drop partitions whose sessions have all idle-expired,
	// so a client rotating throwaway keys/labels can't grow the map without
	// bound. Throttled to at most once per idleGap - a full-map sweep is
	// O(partitions) and must not run on the per-request hot path. The id
	// sequence lives in t.sequence and survives the drop, so a recreated
	// partition never reuses an id.
	if now.Sub(t.lastSweep) >= t.idleGap {
		t.sweepLocked(now)
	}

	pk := convoKey(client, keyHash)
	p := t.parts[pk]
	if p == nil {
		// Do not turn a client-controlled partition churn at the cap into an
		// unthrottled full-map sweep. The oldest-entry eviction below still
		// bounds memory; the periodic throttle above owns map-wide cleanup.
		if len(t.parts) >= conversationPartitionsMax && now.Sub(t.lastSweep) >= t.idleGap {
			t.sweepLocked(now)
		}
		for len(t.parts) >= conversationPartitionsMax && len(t.order) > 0 {
			oldest := t.order[0]
			t.order = t.order[1:]
			if _, live := t.parts[oldest]; live {
				delete(t.parts, oldest)
			}
		}
		p = &convoPartition{}
		t.parts[pk] = p
		t.order = append(t.order, pk)
	}

	// Find the open conversation this request continues: the one with the
	// greatest lastTurn still <= totalTurns that hasn't gone idle.
	var best *convoSession
	for _, s := range p.sessions {
		if now.Sub(s.lastAt) > t.idleGap {
			continue // idle-expired; a fresh request starts a new conversation
		}
		if totalTurns >= s.lastTurn {
			if best == nil || s.lastTurn > best.lastTurn {
				best = s
			}
		}
	}

	if best == nil {
		best = t.newSession(p, pk, now)
	}
	best.lastTurn = max(best.lastTurn, totalTurns)
	best.lastAt = now
	return best.id
}

// sweepLocked drops partitions whose sessions all idle-expired and rebuilds
// the creation-order index from the survivors. Callers hold mu.
func (t *ConversationTracker) sweepLocked(now time.Time) {
	t.lastSweep = now
	for k, part := range t.parts {
		alive := false
		for _, s := range part.sessions {
			if now.Sub(s.lastAt) <= t.idleGap {
				alive = true
				break
			}
		}
		if !alive {
			delete(t.parts, k)
		}
	}
	t.order = t.order[:0]
	for k := range t.parts {
		t.order = append(t.order, k)
	}
}

// newSession creates a conversation under the partition, evicting the oldest
// idle session if the partition is over capacity.
func (t *ConversationTracker) newSession(p *convoPartition, pk string, now time.Time) *convoSession {
	// Evict idle-expired sessions first (cheap GC under the lock).
	alive := p.sessions[:0]
	for _, s := range p.sessions {
		if now.Sub(s.lastAt) <= t.idleGap {
			alive = append(alive, s)
		}
	}
	p.sessions = alive
	// Hard cap: if still too many open (all recently active), drop the oldest.
	if len(p.sessions) >= t.maxOpen {
		p.sessions = p.sessions[len(p.sessions)-t.maxOpen+1:]
	}
	t.sequence++
	s := &convoSession{
		id:     convoID(pk, t.sequence),
		lastAt: now,
	}
	p.sessions = append(p.sessions, s)
	return s
}

// convoIDHexChars is the retained hash width: 16 hexadecimal characters, 64
// bits. The former 40-bit suffix reached about a 1% birthday-collision
// probability near 150k generated ids, merging unrelated histories in the
// dashboard; 64 bits keeps accidental collisions negligible at the tracker's
// supported scale. The c- prefix keeps the id namespaced and opaque.
const convoIDHexChars = 16

// convoID derives a short, stable, opaque conversation id from the partition
// key and a process-lifetime sequence number.
func convoID(pk string, seq uint64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", pk, seq)))
	return "c-" + hex.EncodeToString(h[:])[:convoIDHexChars]
}
