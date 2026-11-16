package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// A hold's HTTP surface carries the same five scope dimensions a limit does,
// and the state document echoes them, so the console can offer one scope strip
// for every rule kind.
func TestPauseScopeDepthOverHTTP(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	defer p.applyHolds(nil)
	body := `{"paused":true,"models":["scoped-model"],"key_hashes":["` + strings.Repeat("a", 64) + `"],"conversations":["conv-1"],"duration":"1h"}`
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause", strings.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", rr.Code, rr.Body.String())
	}
	st := pauseJSON(t, rr)
	holds, _ := st["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("holds = %v", st["holds"])
	}
	hold, _ := holds[0].(map[string]any)
	for _, dim := range []string{"models", "key_hashes", "conversations"} {
		names, _ := hold[dim].([]any)
		if len(names) != 1 {
			t.Fatalf("%s = %v, want one value", dim, hold[dim])
		}
	}
	// The scheduler parks exactly the request identity the hold names.
	parked := p.scheduler.HoldsTarget(scheduler.PolicyTarget{
		Model: "scoped-model", KeyHash: strings.Repeat("a", 64), Conversation: "conv-1",
	})
	if !parked {
		t.Fatal("a hold naming model, key and conversation does not park them")
	}
	if p.scheduler.HoldsTarget(scheduler.PolicyTarget{Model: "other-model",
		KeyHash: strings.Repeat("a", 64), Conversation: "conv-1"}) {
		t.Fatal("a model-scoped hold parked a different model")
	}

	// An edit that omits a dimension preserves it; an explicit empty array
	// clears only that dimension.
	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"id":"`+hold["id"].(string)+`","models":[]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("edit = %d: %s", rr.Code, rr.Body.String())
	}
	edited, _ := pauseJSON(t, rr)["holds"].([]any)[0].(map[string]any)
	if names, _ := edited["models"].([]any); len(names) != 0 {
		t.Fatalf("models = %v, want cleared", edited["models"])
	}
	if names, _ := edited["conversations"].([]any); len(names) != 1 {
		t.Fatalf("conversations = %v, want preserved", edited["conversations"])
	}

	// all still refuses a narrower scope: accepting both would install the
	// global hold while the caller believed it was narrowed.
	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"all":true,"models":["m"]}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("all+models = %d, want 400", rr.Code)
	}
}
