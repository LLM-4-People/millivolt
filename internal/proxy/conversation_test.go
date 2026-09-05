package proxy

import (
	"testing"
	"time"
)

func TestConversationMonotonicSameConvo(t *testing.T) {
	ct := NewConversationTracker(30*time.Minute, 64)
	now := time.Now()
	// A conversation grows its turn count: all map to one id.
	a := ct.Assign("cli", "k", "", 5, now)
	b := ct.Assign("cli", "k", "", 8, now.Add(time.Minute))
	c := ct.Assign("cli", "k", "", 12, now.Add(2*time.Minute))
	if a != b || b != c {
		t.Errorf("monotonic growth split into %q %q %q, want one conversation", a, b, c)
	}
}

func TestConversationResetStartsNew(t *testing.T) {
	ct := NewConversationTracker(30*time.Minute, 64)
	now := time.Now()
	a := ct.Assign("cli", "k", "", 12, now)
	b := ct.Assign("cli", "k", "", 3, now.Add(time.Minute)) // reset -> new convo
	if a == b {
		t.Errorf("turn-count reset should start a new conversation, got same id %q", a)
	}
}

func TestConversationIdleGapSplits(t *testing.T) {
	ct := NewConversationTracker(30*time.Minute, 64)
	now := time.Now()
	a := ct.Assign("cli", "k", "", 5, now)
	b := ct.Assign("cli", "k", "", 6, now.Add(31*time.Minute)) // past idle gap
	if a == b {
		t.Errorf("idle gap exceeded should start a new conversation, got same id %q", a)
	}
	// Within the gap, it continues.
	c := ct.Assign("cli", "k", "", 7, now.Add(31*time.Minute).Add(5*time.Minute))
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
	x1 := ct2.Assign("cli", "k", "", 10, n)                    // convo X, lastTurn 10
	y1 := ct2.Assign("cli", "k", "", 4, n.Add(time.Second))    // reset below 10 -> new convo Y
	x2 := ct2.Assign("cli", "k", "", 12, n.Add(2*time.Second)) // grows X (12>=10)
	y2 := ct2.Assign("cli", "k", "", 5, n.Add(3*time.Second))  // grows Y (5>=4)
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

func TestConversationExplicitHeaderWins(t *testing.T) {
	ct := NewConversationTracker(30*time.Minute, 64)
	now := time.Now()
	a := ct.Assign("cli", "k", "my-task-123", 0, now)
	b := ct.Assign("cli", "k", "my-task-123", 0, now.Add(time.Hour)) // explicit always groups
	if a != "s:my-task-123" || b != "s:my-task-123" {
		t.Errorf("explicit session id not honored: %q %q", a, b)
	}
	// Auto and explicit never collide even with same underlying text.
	c := ct.Assign("cli", "k", "", 1, now)
	if c == a {
		t.Errorf("auto id %q collided with explicit %q", c, a)
	}
}

func TestConversationDifferentKeysSeparate(t *testing.T) {
	ct := NewConversationTracker(30*time.Minute, 64)
	now := time.Now()
	a := ct.Assign("cli", "keyA", "", 5, now)
	b := ct.Assign("cli", "keyB", "", 6, now.Add(time.Second)) // different key
	if a == b {
		t.Errorf("different API keys share a conversation %q, want separate", a)
	}
}
