package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// maxDeclaredSessionBytes bounds retained relationship identifiers. This is
// an internal safety guardrail for the opt-in parent declaration, not a
// truncation policy or a new limit on legacy session-only requests.
const maxDeclaredSessionBytes = 512

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
		if id == "" || len(id) > maxDeclaredSessionBytes || !utf8.ValidString(id) || strings.ContainsFunc(id, func(r rune) bool { return r < ' ' || r == 0x7f }) {
			return "", "", fmt.Errorf("declared session identifiers must be nonempty printable text of at most %d bytes", maxDeclaredSessionBytes)
		}
	}
	if session == parent {
		return "", "", fmt.Errorf("a session cannot declare itself as its parent")
	}
	return session, parent, nil
}

func explicitConversationID(id string) string { return "s:" + id }

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
// X-Proxy-Session) wins when present; otherwise the request is auto-grouped by
// turn count. totalTurns is turns_user+turns_assistant+turns_tool.
func (t *ConversationTracker) Assign(client, keyHash, explicitID string, totalTurns int, now time.Time) string {
	if explicitID != "" {
		// Namespaced so an explicit id can never collide with an auto one.
		return explicitConversationID(explicitID)
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
