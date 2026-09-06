package scheduler

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestUnavailableSettlementPreservesReservationExactlyOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Options{})
		s.SetThrottle(Throttle{Provider: "up.example", Limit: Limit{Concurrency: 1, Tokens: 100, TokWindow: time.Hour}})
		release, err := s.AcquireWith(context.Background(), "group", 0, WaiterHooks{Provider: "up.example", EstTokens: 40})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() { release(TokensUnavailable) })
		}
		wg.Wait()
		release(0)
		g := s.gate("up.example")
		if got := g.snapshot(0); got.TokRemaining != 60 || got.InFlight != 0 || g.reserved != 0 || s.Stats().InFlight != 0 {
			t.Fatalf("unavailable settlement refunded/debited or leaked lease: %+v reserved=%d", got, g.reserved)
		}
	})
}
