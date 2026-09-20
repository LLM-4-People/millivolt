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

// maxDeclaredSessionBytes bounds retained relationship identifiers. This is
// an internal safety guardrail for the opt-in parent declaration, not a
// truncation policy or a new limit on legacy session-only requests.
const maxDeclaredSessionBytes = 512

// validDeclaredSession is the single owner of the retained-identifier bound:
// non-empty, at most maxDeclaredSessionBytes, valid UTF-8, no control bytes.
// requestConversation rejects a violating header declaration at the request
// boundary; the sub-conversation extraction drops a violating tracked value
// (never a 400 - the body is passthrough payload).
func validDeclaredSession(id string) bool {
	return id != "" && len(id) <= maxDeclaredSessionBytes && utf8.ValidString(id) &&
		!strings.ContainsFunc(id, func(r rune) bool { return r < ' ' || r == 0x7f })
}

// requestConversation owns the optional parent declaration boundary. A parent
// requires the child's explicit identity; ordinary session-only requests keep
// their existing semantics. These identifiers are client claims, not proof of
// agent spawning, and must never be inferred from messages or response IDs.
func requestConversation(h http.Header) (session, parent string, err error) {
	session = strings.TrimSpace(h.Get(hdrSession))
	parents, declared := h[http.CanonicalHeaderKey(hdrParentSession)]
	if !declared {
		return session, "", nil
	}
	if len(parents) != 1 || len(h.Values(hdrSession)) != 1 {
		return "", "", fmt.Errorf("%s requires exactly one %s and one parent value", hdrParentSession, hdrSession)
	}
	parent = strings.TrimSpace(parents[0])
	for _, id := range []string{session, parent} {
		if !validDeclaredSession(id) {
			return "", "", fmt.Errorf("declared session identifiers must be nonempty printable text of at most %d bytes", maxDeclaredSessionBytes)
		}
	}
	if session == parent {
		return "", "", fmt.Errorf("a session cannot declare itself as its parent")
	}
	return session, parent, nil
}

func explicitConversationID(id string) string { return "s:" + id }

// trackedConversationID namespaces a sub-conversation param identity beside
// explicitConversationID's s: sessions: a k: id can never collide with an
// explicit session or a c- auto grouping.
func trackedConversationID(id string) string { return "k:" + id }

// subConversationScanMax bounds the raw byte scan of one tracked param's
// string value. The bound is judged on raw bytes, before the decoded value is
// trimmed: a value with no trimmable edges that still satisfies
// maxDeclaredSessionBytes never spans more raw bytes than this - every JSON
// escape sequence spends at least two raw bytes per decoded byte (one
// six-byte \uXXXX escape per decoded byte is the worst case), plus the two
// framing quotes. A value whose trimmable edges push its raw span past the
// window is dropped conservatively (still never a 400 - the body is
// passthrough payload, the same deny-by-drop precedent as an invalid value).
// Internal safety guardrail, not user-tunable.
const subConversationScanMax = 6*maxDeclaredSessionBytes + 2

// resolveSubConversation is the sub_conversations request-path owner: it
// resolves the classified client's configured entry and extracts the tracked
// value from the already-buffered request body. No entries configured, or no
// exact client leaf match, returns (nil, "") - the feature-off path leaves the
// request byte-identical and untracked. For a matched entry the configured
// params are checked in order and the first one whose value tail supplies a
// decodable string wins; a supplied value that fails the declared-session
// bound is dropped (the request stays on automatic grouping), never a 400 -
// the body is passthrough payload, the configToken deny-by-drop precedent.
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
	// One backslash probe shared by every param's negative pre-gate: the
	// param grammar forbids quote, backslash and control bytes in the name
	// itself, so a key can only reach the wire spelled literally or through
	// escape sequences.
	anyEscape := bytes.ContainsRune(body, '\\')
	for _, name := range entry.Params {
		if v, present := subConversationParam(body, name, anyEscape); present {
			return entry, v
		}
	}
	return entry, ""
}

// subConversationParam locates one configured param's string value among the
// buffered body's TOP-LEVEL object keys - a request body param is top-level
// by product semantics, so an occurrence of the same spelling nested inside
// a value is neither tracked nor stripped. present reports that the param
// supplied the tracked value: the top-level locate found the key and the
// value tail decodes as a bounded string. A supplied value that fails the
// declared-session bound comes back present with the empty value - the first
// present param wins and its invalid value is dropped, never a fall-through
// to a later param, so the configured order stays presence-ordered, never
// value-quality-ordered.
func subConversationParam(body []byte, name string, anyEscape bool) (value string, present bool) {
	// Keep param-free regions on the vectorized negative path (the
	// HasErrorKey pre-gate class): a body without the literal quoted spelling
	// and without any escape sequence cannot carry the key.
	if !bytes.Contains(body, []byte(`"`+name+`"`)) && !anyEscape {
		return "", false
	}
	tail := topLevelJSONKey(body, name)
	if len(tail) == 0 || tail[0] != '"' {
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
		// Unterminated within the scan cap: too long for any bounded value.
		return "", false
	}
	token := window[:end+1]
	// The standard decoder coerces invalid UTF-8 inside strings to U+FFFD;
	// the identity bound must judge the client's actual bytes, so validate
	// before decoding.
	if !utf8.Valid(token[1:end]) {
		return "", false
	}
	var s string
	if json.Unmarshal(token, &s) != nil {
		return "", false
	}
	s = strings.TrimSpace(s)
	if !validDeclaredSession(s) {
		return "", true
	}
	return s, true
}

// topLevelJSONKey locates the named key among the members of the body's
// top-level JSON object and returns the input tail beginning at its value;
// nil when the body is not a JSON object or the key is not a top-level
// member. This is the depth-aware sibling of metrics.JSONKey's any-depth
// walk (which stays with the SSE and error-class consumers): keys inside
// nested objects or arrays never match. A lexical scan of the buffered
// bytes, never a second document decode - string escapes are honored, and a
// body too malformed to finish the scan simply locates nothing.
func topLevelJSONKey(data []byte, key string) []byte {
	i := metrics.SkipSpace(data, 0)
	if i >= len(data) || data[i] != '{' {
		return nil
	}
	i = metrics.SkipSpace(data, i+1)
	for {
		if i >= len(data) || data[i] == '}' {
			return nil
		}
		if data[i] != '"' {
			return nil // a member must open with a key string
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
			return nil
		}
		end := i // the key's closing quote
		i = metrics.SkipSpace(data, i+1)
		if i >= len(data) || data[i] != ':' {
			return nil
		}
		i = metrics.SkipSpace(data, i+1)
		if i >= len(data) {
			return nil
		}
		match := string(data[start+1:end]) == key
		if escaped {
			var name string
			match = json.Unmarshal(data[start:end+1], &name) == nil && name == key
		}
		if match {
			return data[i:]
		}
		i = skipJSONValue(data, i)
		if i < 0 {
			return nil
		}
		i = metrics.SkipSpace(data, i)
		if i >= len(data) || data[i] != ',' {
			return nil // not a member separator: the scan is over
		}
		i = metrics.SkipSpace(data, i+1)
	}
}

// skipJSONValue advances past one JSON value - a string, a nested object or
// array tracked as a bracket depth, or a bare number/literal - returning the
// index just past the value, or -1 when the bytes run out inside an
// unterminated structure. Lexical only: it never validates the value.
func skipJSONValue(data []byte, i int) int {
	switch data[i] {
	case '"':
		i++
		for i < len(data) {
			if data[i] == '\\' {
				i += 2
				continue
			}
			if data[i] == '"' {
				return i + 1
			}
			i++
		}
		return -1
	case '{', '[':
		depth := 1
		i++
		for i < len(data) && depth > 0 {
			switch data[i] {
			case '"':
				i++
				for i < len(data) && data[i] != '"' {
					if data[i] == '\\' {
						i++
					}
					i++
				}
				if i >= len(data) {
					return -1
				}
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			i++
		}
		if depth != 0 {
			return -1
		}
		return i
	default:
		// A number or literal (on invalid input, garbage): the next member
		// separator or closing bracket ends the scalar.
		for i < len(data) && data[i] != ',' && data[i] != '}' && data[i] != ']' {
			i++
		}
		return i
	}
}

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
type ConversationTracker struct {
	mu        sync.Mutex
	parts     map[string]*convoPartition
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
	}

	pk := convoKey(client, keyHash)
	p := t.parts[pk]
	if p == nil {
		p = &convoPartition{}
		t.parts[pk] = p
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

// convoID derives a short, stable, opaque conversation id from the partition
// key and a process-lifetime sequence number.
func convoID(pk string, seq uint64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", pk, seq)))
	return "c-" + hex.EncodeToString(h[:])[:10]
}
