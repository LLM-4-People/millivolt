package scheduler

import (
	"testing"
)

// A hold's scope has the same depth as a limit's: model, key and conversation
// narrow it exactly as they narrow a policy, and an unset dimension stays
// "any". Before this, a hold could only name clients and providers, so the
// console could not express "park this model" while a limit could cap it.
func TestHoldScopeDepth(t *testing.T) {
	cases := []struct {
		name  string
		hold  Hold
		t     PolicyTarget
		match bool
	}{
		{"model hit", Hold{Scope: LimitScope{Models: []string{"m1"}}},
			PolicyTarget{Model: "m1", Provider: "p", Client: "c"}, true},
		{"model miss", Hold{Scope: LimitScope{Models: []string{"m1"}}},
			PolicyTarget{Model: "m2", Provider: "p", Client: "c"}, false},
		{"key hit", Hold{Scope: LimitScope{KeyHashes: []string{"k1"}}},
			PolicyTarget{KeyHash: "k1", Client: "c"}, true},
		{"key miss", Hold{Scope: LimitScope{KeyHashes: []string{"k1"}}},
			PolicyTarget{KeyHash: "k2", Client: "c"}, false},
		{"conversation hit", Hold{Scope: LimitScope{Conversations: []string{"v1"}}},
			PolicyTarget{Conversation: "v1", Client: "c"}, true},
		{"conversation miss", Hold{Scope: LimitScope{Conversations: []string{"v1"}}},
			PolicyTarget{Conversation: "v2", Client: "c"}, false},
		{"dimensions AND", Hold{Scope: LimitScope{Models: []string{"m1"}, Clients: []string{"c"}}},
			PolicyTarget{Model: "m1", Client: "other"}, false},
		{"empty dimension is any", Hold{Scope: LimitScope{Models: []string{"m1"}}},
			PolicyTarget{Model: "m1", Client: "anything", Provider: "any"}, true},
	}
	for _, tc := range cases {
		if got := snapHold(tc.hold).matches(tc.t); got != tc.match {
			t.Errorf("%s: matches = %v, want %v", tc.name, got, tc.match)
		}
	}
	// Two holds that name different models over the same provider must coexist.
	s := New(Options{})
	if err := s.AddHold(Hold{Scope: LimitScope{Providers: []string{"p"}, Models: []string{"m1"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddHold(Hold{Scope: LimitScope{Providers: []string{"p"}, Models: []string{"m2"}}}); err != nil {
		t.Fatalf("two model holds on one provider must coexist: %v", err)
	}
	// The same model twice is one target, so the second add conflicts.
	if err := s.AddHold(Hold{Scope: LimitScope{Providers: []string{"p"}, Models: []string{"m1"}}}); err != ErrOverlap {
		t.Fatalf("same selector = %v, want ErrOverlap", err)
	}
	// A provider-only hold overlaps a model hold for the same provider.
	if err := s.AddHold(Hold{Scope: LimitScope{Providers: []string{"p"}}}); err != ErrOverlap {
		t.Fatalf("provider-only over a model hold = %v, want ErrOverlap", err)
	}
}
