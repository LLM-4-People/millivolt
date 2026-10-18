package proxy

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestConversationMonotonicSameConvo(t *testing.T) {
	ct := NewConversationTracker(30*time.Minute, 64)
	now := time.Now()
	// A conversation grows its turn count: all map to one id.
	a := ct.Assign("cli", "k", "", "", 5, now)
	b := ct.Assign("cli", "k", "", "", 8, now.Add(time.Minute))
	c := ct.Assign("cli", "k", "", "", 12, now.Add(2*time.Minute))
	if a != b || b != c {
		t.Errorf("monotonic growth split into %q %q %q, want one conversation", a, b, c)
	}
}

func TestConversationResetStartsNew(t *testing.T) {
	ct := NewConversationTracker(30*time.Minute, 64)
	now := time.Now()
	a := ct.Assign("cli", "k", "", "", 12, now)
	b := ct.Assign("cli", "k", "", "", 3, now.Add(time.Minute)) // reset -> new convo
	if a == b {
		t.Errorf("turn-count reset should start a new conversation, got same id %q", a)
	}
}

func TestConversationIdleGapSplits(t *testing.T) {
	ct := NewConversationTracker(30*time.Minute, 64)
	now := time.Now()
	a := ct.Assign("cli", "k", "", "", 5, now)
	b := ct.Assign("cli", "k", "", "", 6, now.Add(31*time.Minute)) // past idle gap
	if a == b {
		t.Errorf("idle gap exceeded should start a new conversation, got same id %q", a)
	}
	// Within the gap, it continues.
	c := ct.Assign("cli", "k", "", "", 7, now.Add(31*time.Minute).Add(5*time.Minute))
	if c != b {
		t.Errorf("within idle gap should continue conversation %q, got %q", b, c)
	}
}

func TestConversationConcurrentInterleaved(t *testing.T) {
	// Two concurrent conversations from the SAME client+key, interleaved.
	// With nearest-prefix matching, a second conversation is opened when a
	// request's turn count RESETS below the current conversation's last count,
	// while a still-higher count continues the first.
	ct2 := NewConversationTracker(30*time.Minute, 64)
	n := time.Now()
	x1 := ct2.Assign("cli", "k", "", "", 10, n)                    // convo X, lastTurn 10
	y1 := ct2.Assign("cli", "k", "", "", 4, n.Add(time.Second))    // reset below 10 -> new convo Y
	x2 := ct2.Assign("cli", "k", "", "", 12, n.Add(2*time.Second)) // grows X (12>=10)
	y2 := ct2.Assign("cli", "k", "", "", 5, n.Add(3*time.Second))  // grows Y (5>=4)
	if x1 != x2 {
		t.Errorf("convo X split: %q vs %q", x1, x2)
	}
	if y1 != y2 {
		t.Errorf("convo Y split: %q vs %q", y1, y2)
	}
	if x1 == y1 {
		t.Errorf("concurrent conversations share id %q, want distinct", x1)
	}
}

// TestConversationCapSweepThrottled pins the cap branch to the same sweep
// throttle as the normal path: adding a partition at the cap evicts the
// oldest entry but must not run an O(partitions) scan on every request.
func TestConversationCapSweepThrottled(t *testing.T) {
	ct := NewConversationTracker(time.Hour, 4)
	now := time.Now()
	for i := 0; i < conversationPartitionsMax; i++ {
		ct.Assign("cli", fmt.Sprintf("partition-%d", i), "", "", 1, now)
	}
	if len(ct.parts) != conversationPartitionsMax {
		t.Fatalf("filled partitions = %d, want %d", len(ct.parts), conversationPartitionsMax)
	}
	ct.lastSweep = now
	ct.Assign("cli", "partition-new", "", "", 1, now.Add(time.Second))
	if !ct.lastSweep.Equal(now) {
		t.Fatalf("cap insertion swept at %v, want the throttle to keep %v", ct.lastSweep, now)
	}
	if len(ct.parts) != conversationPartitionsMax {
		t.Fatalf("partitions after eviction = %d, want %d", len(ct.parts), conversationPartitionsMax)
	}
}

func TestConversationExplicitHeaderWins(t *testing.T) {
	ct := NewConversationTracker(30*time.Minute, 64)
	now := time.Now()
	a := ct.Assign("cli", "k", "my-task-123", "", 0, now)
	b := ct.Assign("cli", "k", "my-task-123", "", 0, now.Add(time.Hour)) // explicit always groups
	if a != "s:my-task-123" || b != "s:my-task-123" {
		t.Errorf("explicit session id not honored: %q %q", a, b)
	}
	// Auto and explicit never collide even with same underlying text.
	c := ct.Assign("cli", "k", "", "", 1, now)
	if c == a {
		t.Errorf("auto id %q collided with explicit %q", c, a)
	}
}

func TestConversationDifferentKeysSeparate(t *testing.T) {
	ct := NewConversationTracker(30*time.Minute, 64)
	now := time.Now()
	a := ct.Assign("cli", "keyA", "", "", 5, now)
	b := ct.Assign("cli", "keyB", "", "", 6, now.Add(time.Second)) // different key
	if a == b {
		t.Errorf("different API keys share a conversation %q, want separate", a)
	}
}

// TestConversationNamespaceOwnersBoundInvalidIDs pins the defensive namespace
// owners: a valid id keeps its exact s:/k: form, while a direct Assign with an
// invalid or overlong id hashes the whole value, so distinct values cannot
// collide through truncation and the retained identifier stays bounded.
func TestConversationNamespaceOwnersBoundInvalidIDs(t *testing.T) {
	ct := NewConversationTracker(30*time.Minute, 64)
	now := time.Now()
	prefix := strings.Repeat("x", maxDeclaredSessionBytes+10)
	explicitA := ct.Assign("cli", "k", prefix+"-a", "", 0, now)
	explicitB := ct.Assign("cli", "k", prefix+"-b", "", 0, now)
	trackedA := ct.Assign("cli", "k", "", prefix+"-a", 0, now)
	trackedB := ct.Assign("cli", "k", "", prefix+"-b", 0, now)
	if explicitA == explicitB || trackedA == trackedB {
		t.Fatalf("hashed identifiers collided: %q/%q and %q/%q", explicitA, explicitB, trackedA, trackedB)
	}
	for _, got := range []string{explicitA, explicitB} {
		if !strings.HasPrefix(got, "s:h:") || len(got) > maxDeclaredSessionBytes+2 {
			t.Fatalf("explicit hashed id = %q (len %d)", got, len(got))
		}
	}
	for _, got := range []string{trackedA, trackedB} {
		if !strings.HasPrefix(got, "k:h:") || len(got) > maxDeclaredSessionBytes+2 {
			t.Fatalf("tracked hashed id = %q (len %d)", got, len(got))
		}
	}
	if got := ct.Assign("cli", "k", "task", "", 0, now); got != "s:task" {
		t.Fatalf("valid explicit id = %q, want s:task", got)
	}
	if got := ct.Assign("cli", "k", "", "tracked", 0, now); got != "k:tracked" {
		t.Fatalf("valid tracked id = %q, want k:tracked", got)
	}
}

// TestConversationAutoID64BitFormat pins the widened auto identity: the c-
// namespace stays opaque, the suffix is 16 hex characters (64 bits), and a
// fixture pair that collided under the former 10-character (40-bit) suffix now
// stays distinct. The fixture is deterministic, so a regression to the narrow
// width fails here without a probabilistic collision search.
func TestConversationAutoID64BitFormat(t *testing.T) {
	ct := NewConversationTracker(30*time.Minute, 64)
	now := time.Now()
	id := ct.Assign("cli", "k", "", "", 1, now)
	if !strings.HasPrefix(id, "c-") || len(id) != len("c-")+convoIDHexChars {
		t.Fatalf("auto conversation id = %q, want c- plus %d hex characters", id, convoIDHexChars)
	}
	if _, err := strconv.ParseUint(id[len("c-"):], 16, 64); err != nil {
		t.Fatalf("auto conversation suffix %q is not a 64-bit hexadecimal value: %v", id[len("c-"):], err)
	}
	if got := convoID("pk", 7); got != convoID("pk", 7) {
		t.Fatalf("convoID is not stable for one input: %q vs %q", got, convoID("pk", 7))
	}
	if convoID("pk", 7) == convoID("pk", 8) {
		t.Fatal("distinct sequence numbers share an auto conversation id")
	}

	const formerHexChars = 10
	a := convoID("collision-fixture", 886210)
	b := convoID("collision-fixture", 1063374)
	if a == b {
		t.Fatalf("fixture pair still collides at the retained width: %q", a)
	}
	if a[:len("c-")+formerHexChars] != b[:len("c-")+formerHexChars] {
		t.Fatalf("fixture pair no longer exercises the former 40-bit suffix: %q vs %q", a, b)
	}
}
