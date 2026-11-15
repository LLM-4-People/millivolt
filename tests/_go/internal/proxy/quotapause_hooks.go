package proxy

import (
	"slices"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// A quota hold is an operator hold like any other: it must pin its provider in
// the known-name vocabulary through the shared commit owner, and releasing it
// must release the pin again.
func TestQuotaHoldPinsItsProvider(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	if !p.addQuotaHold(scheduler.PolicyTarget{Provider: "quota-provider"}, "quota") {
		t.Fatal("addQuotaHold refused")
	}
	if !slices.Contains(pinnedNames(&p.pause.providers), "quota-provider") {
		t.Fatalf("quota hold did not pin its provider: %v", pinnedNames(&p.pause.providers))
	}
	if len(p.pause.holds) != 1 {
		t.Fatalf("quota hold not installed: %+v", p.pause.holds)
	}
	if err := p.removePause(p.pause.holds[0].ID, p.pause.holds[0].Until); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pinnedNames(&p.pause.providers), "quota-provider") {
		t.Fatalf("resumed quota hold kept its pin: %v", pinnedNames(&p.pause.providers))
	}
}
