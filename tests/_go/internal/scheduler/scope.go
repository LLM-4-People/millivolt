package scheduler

import (
	"context"
	"testing"
	"time"
)

// acquireBlocked reports whether a second acquisition to the same provider is
// parked. It runs the acquisition in a goroutine and waits a short, bounded
// interval; a grant closes done.
func acquireBlocked(t *testing.T, s *Scheduler, key string, hooks WaiterHooks) (release func(), blocked bool) {
	t.Helper()
	done := make(chan Release, 1)
	go func() {
		r, err := s.AcquireWith(context.Background(), key, 0, hooks)
		if err != nil {
			done <- nil
			return
		}
		done <- r
	}()
	select {
	case r := <-done:
		if r == nil {
			return nil, false
		}
		return func() { r(0) }, false
	case <-time.After(40 * time.Millisecond):
		return nil, true
	}
}

func TestScopedPolicyMatchesOnlySelectedModel(t *testing.T) {
	s := New(Options{})
	s.SetPolicy(Throttle{
		Scope: LimitScope{Models: []string{"expensive-model"}},
		Limit: Limit{Concurrency: 1},
	})
	first, err := s.AcquireWith(context.Background(), "p|k1", 0,
		WaiterHooks{Provider: "p", Model: "expensive-model"})
	if err != nil {
		t.Fatal(err)
	}
	defer first(0)

	if _, blocked := acquireBlocked(t, s, "p|k2", WaiterHooks{Provider: "p", Model: "expensive-model"}); !blocked {
		t.Fatal("second matching model was admitted past a model-scoped concurrency cap")
	}
	other, err := s.AcquireWith(context.Background(), "p|k3", 0,
		WaiterHooks{Provider: "p", Model: "cheap-model"})
	if err != nil {
		t.Fatal(err)
	}
	other(0)
}

func TestScopedPolicyMatchesClientKeyAndConversation(t *testing.T) {
	cases := []struct {
		name   string
		scope  LimitScope
		match  WaiterHooks
		reject WaiterHooks
	}{
		{
			name:   "client",
			scope:  LimitScope{Clients: []string{"agent-a"}},
			match:  WaiterHooks{Provider: "p", Client: "agent-a"},
			reject: WaiterHooks{Provider: "p", Client: "agent-b"},
		},
		{
			name:   "key hash",
			scope:  LimitScope{KeyHashes: []string{"aaaa"}},
			match:  WaiterHooks{Provider: "p", KeyHash: "aaaa"},
			reject: WaiterHooks{Provider: "p", KeyHash: "bbbb"},
		},
		{
			name:   "conversation",
			scope:  LimitScope{Conversations: []string{"conv-a"}},
			match:  WaiterHooks{Provider: "p", Conversation: "conv-a"},
			reject: WaiterHooks{Provider: "p", Conversation: "conv-b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Options{})
			s.SetPolicy(Throttle{Scope: tc.scope, Limit: Limit{Concurrency: 1}})
			first, err := s.AcquireWith(context.Background(), "p|k1", 0, tc.match)
			if err != nil {
				t.Fatal(err)
			}
			defer first(0)
			if _, blocked := acquireBlocked(t, s, "p|k2", tc.match); !blocked {
				t.Fatal("matching request was admitted past a scoped concurrency cap")
			}
			other, err := s.AcquireWith(context.Background(), "p|k3", 0, tc.reject)
			if err != nil {
				t.Fatal(err)
			}
			other(0)
		})
	}
}

func TestGlobalPolicyAppliesToEveryProvider(t *testing.T) {
	s := New(Options{})
	s.SetPolicy(Throttle{Limit: Limit{Concurrency: 1}})
	first, err := s.AcquireWith(context.Background(), "a|k", 0, WaiterHooks{Provider: "a"})
	if err != nil {
		t.Fatal(err)
	}
	defer first(0)
	if _, blocked := acquireBlocked(t, s, "b|k", WaiterHooks{Provider: "b"}); !blocked {
		t.Fatal("global policy admitted a second provider past its concurrency cap")
	}
}

func TestEveryMatchingPolicyChargesAndSettles(t *testing.T) {
	s := New(Options{})
	s.SetPolicy(Throttle{Scope: LimitScope{Providers: []string{"p"}}, Limit: Limit{Concurrency: 2}})
	s.SetPolicy(Throttle{Scope: LimitScope{Models: []string{"m"}}, Limit: Limit{Concurrency: 2}})

	release, err := s.AcquireWith(context.Background(), "p|k", 0,
		WaiterHooks{Provider: "p", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range s.ListThrottles() {
		if info.InFlight != 1 {
			t.Fatalf("policy %s in-flight = %d, want 1 after one matching admission", info.ID, info.InFlight)
		}
	}
	release(0)
	for _, info := range s.ListThrottles() {
		if info.InFlight != 0 {
			t.Fatalf("policy %s in-flight = %d, want 0 after settlement", info.ID, info.InFlight)
		}
	}
}

func TestSchedulePolicyActiveOnlyInsideWindow(t *testing.T) {
	now := time.Now()
	minute := now.Hour()*60 + now.Minute()
	activeStart := (minute + minutesPerDay - 1) % minutesPerDay
	activeEnd := (minute + 2) % minutesPerDay
	inactiveStart := (minute + 6*60) % minutesPerDay
	inactiveEnd := (inactiveStart + 1) % minutesPerDay
	days := []int{0, 1, 2, 3, 4, 5, 6}

	s := New(Options{})
	s.SetPolicy(Throttle{
		Scope:    LimitScope{Providers: []string{"p"}},
		Schedule: LimitSchedule{Days: days, StartMin: activeStart, EndMin: activeEnd},
		Limit:    Limit{Concurrency: 1},
	})
	first, err := s.AcquireWith(context.Background(), "p|k1", 0, WaiterHooks{Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer first(0)
	if _, blocked := acquireBlocked(t, s, "p|k2", WaiterHooks{Provider: "p"}); !blocked {
		t.Fatal("inside its window the policy did not apply")
	}

	s2 := New(Options{})
	s2.SetPolicy(Throttle{
		Scope:    LimitScope{Providers: []string{"p"}},
		Schedule: LimitSchedule{Days: days, StartMin: inactiveStart, EndMin: inactiveEnd},
		Limit:    Limit{Concurrency: 1},
	})
	a, err := s2.AcquireWith(context.Background(), "p|k1", 0, WaiterHooks{Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer a(0)
	b, err := s2.AcquireWith(context.Background(), "p|k2", 0, WaiterHooks{Provider: "p"})
	if err != nil {
		t.Fatal(err)
	}
	b(0)
}

func TestLimitScopeNormalizationAndIdentity(t *testing.T) {
	scope := NormalizeLimitScope(LimitScope{
		Providers: []string{" b ", "a", "a", ""},
		Clients:   []string{"z", "a"},
	})
	if len(scope.Providers) != 2 || scope.Providers[0] != "a" || scope.Providers[1] != "b" {
		t.Fatalf("providers = %v, want [a b]", scope.Providers)
	}
	if len(scope.Clients) != 2 || scope.Clients[0] != "a" || scope.Clients[1] != "z" {
		t.Fatalf("clients = %v, want [a z]", scope.Clients)
	}
	same := NormalizeLimitScope(LimitScope{Providers: []string{"a", "b"}, Clients: []string{"a", "z"}})
	if PolicyID(scope, LimitSchedule{}) != PolicyID(same, LimitSchedule{}) {
		t.Fatal("equivalent scopes produced different policy identities")
	}
	other := LimitScope{Providers: []string{"a"}}
	if PolicyID(scope, LimitSchedule{}) == PolicyID(other, LimitSchedule{}) {
		t.Fatal("different scopes produced the same policy identity")
	}
	if got := PolicyID(LimitScope{Providers: []string{"a"}}, LimitSchedule{}); got == PolicyID(LimitScope{Providers: []string{"a"}}, LimitSchedule{StartMin: 60, EndMin: 120}) {
		t.Fatal("schedule change did not change policy identity")
	}
}

func TestLimitScheduleActiveSpanningMidnight(t *testing.T) {
	monday := time.Date(2026, time.January, 5, 23, 30, 0, 0, time.Local) // Monday
	schedule := LimitSchedule{Days: []int{1}, StartMin: 22 * 60, EndMin: 2 * 60}
	if !schedule.Active(monday) {
		t.Fatal("23:30 Monday was outside a Monday 22:00-02:00 window")
	}
	tuesday := time.Date(2026, time.January, 6, 1, 0, 0, 0, time.Local)
	if !schedule.Active(tuesday) {
		t.Fatal("01:00 Tuesday was outside the overnight portion of a Monday window")
	}
	if schedule.Active(time.Date(2026, time.January, 6, 12, 0, 0, 0, time.Local)) {
		t.Fatal("noon Tuesday fell inside a Monday 22:00-02:00 window")
	}
}
