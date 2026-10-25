package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

func TestHandlePauseReplaceKeepsID(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"clients":["client-a"],"duration":"1h","max_queued":2}`)))
	if rr.Code != 200 {
		t.Fatalf("add = %d %s", rr.Code, rr.Body.Bytes())
	}
	st := pauseJSON(t, rr)
	holds, _ := st["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("holds = %v", st["holds"])
	}
	id := holds[0].(map[string]any)["id"].(string)
	until := holds[0].(map[string]any)["until"]
	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"id":"`+id+`","clients":["client-a","client-b"],"duration":"1h","max_queued":5}`)))
	if rr.Code != 200 {
		t.Fatalf("replace = %d %s", rr.Code, rr.Body.Bytes())
	}
	st = pauseJSON(t, rr)
	holds, _ = st["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("after replace holds = %v", st["holds"])
	}
	h := holds[0].(map[string]any)
	if h["id"] != id {
		t.Fatalf("id changed %v -> %v", id, h["id"])
	}
	if h["until"] != until {
		t.Fatalf("same duration reset until %v -> %v", until, h["until"])
	}
	if h["max_queued"] != float64(5) {
		t.Fatalf("max_queued = %v, want 5", h["max_queued"])
	}
	if !p.scheduler.Holds("client-a") || !p.scheduler.Holds("client-b") {
		t.Fatal("replaced hold did not cover both clients")
	}
	if n := len(p.PauseSnapshot()["holds"].([]map[string]any)); n != 1 {
		t.Fatalf("holds = %d, want 1 (update, not add)", n)
	}
}

// TestHandlePauseReplaceFrontendUpdateKeepsProviderScope posts the exact
// dashboard Update JSON (paused/all/new/clients/providers/id/max_queued).
// A no-op edit of a provider-only hold must not become All or client-wide.
func TestHandlePauseReplaceFrontendUpdateKeepsProviderScope(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"all":false,"new":false,"clients":[],"providers":["gamma.example"],"duration":"","max_queued":0}`)))
	if rr.Code != 200 {
		t.Fatalf("add = %d %s", rr.Code, rr.Body.Bytes())
	}
	st := pauseJSON(t, rr)
	holds, _ := st["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("holds = %v", st["holds"])
	}
	id := holds[0].(map[string]any)["id"].(string)
	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"all":false,"new":false,"clients":[],"providers":["gamma.example"],"duration":"","id":"`+id+`","max_queued":0}`)))
	if rr.Code != 200 {
		t.Fatalf("update = %d %s", rr.Code, rr.Body.Bytes())
	}
	st = pauseJSON(t, rr)
	holds, _ = st["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("after update holds = %v", st["holds"])
	}
	h := holds[0].(map[string]any)
	if h["id"] != id {
		t.Fatalf("id changed %v -> %v", id, h["id"])
	}
	if h["all"] != false {
		t.Fatalf("update set all=%v, want false", h["all"])
	}
	if !p.scheduler.HoldsTarget("client-a", "gamma.example") {
		t.Fatal("provider hold dropped")
	}
	if p.scheduler.HoldsTarget("client-a", "beta.example") || p.scheduler.Holds("client-a") {
		t.Fatal("update applied the hold to every request")
	}
	rel, err := p.scheduler.AcquireWith(context.Background(), "k", 0, scheduler.WaiterHooks{
		Client: "client-a", Provider: "beta.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	rel(0)
}

// TestHandlePauseReplaceDurationOnlyKeepsProviderScope is the dashboard
// "I only changed the timer" path: same named providers, new duration.
// Until must move; other providers must still grant.
func TestHandlePauseReplaceDurationOnlyKeepsProviderScope(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"all":false,"new":false,"clients":[],"providers":["gamma.example"],"duration":"","max_queued":0}`)))
	if rr.Code != 200 {
		t.Fatalf("add = %d %s", rr.Code, rr.Body.Bytes())
	}
	id := pauseJSON(t, rr)["holds"].([]any)[0].(map[string]any)["id"].(string)
	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"all":false,"new":false,"clients":[],"providers":["gamma.example"],"duration":"15m","id":"`+id+`","max_queued":0}`)))
	if rr.Code != 200 {
		t.Fatalf("timer update = %d %s", rr.Code, rr.Body.Bytes())
	}
	st := pauseJSON(t, rr)
	h := st["holds"].([]any)[0].(map[string]any)
	if h["id"] != id || h["all"] != false || h["duration"] != "15m" || h["until"] == nil {
		t.Fatalf("timer update mutated hold %v", h)
	}
	provs, _ := h["providers"].([]any)
	if len(provs) != 1 || provs[0] != "gamma.example" {
		t.Fatalf("providers = %v, want [gamma.example]", h["providers"])
	}
	if p.scheduler.HoldsTarget("client-a", "beta.example") || p.scheduler.Holds("client-a") {
		t.Fatal("timer-only update applied the hold to every request")
	}
	if !p.scheduler.HoldsTarget("client-a", "gamma.example") {
		t.Fatal("timer-only update dropped the provider hold")
	}
	rel, err := p.scheduler.AcquireWith(context.Background(), "k", 0, scheduler.WaiterHooks{
		Client: "client-a", Provider: "beta.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	rel(0)
}

// TestHandlePauseReplaceOmittedScopeKeepsPrevious pins deny-by-default on
// edit: {"paused":true,"id"} without restated scope must keep the hold,
// not promote it to All (compat {"paused":true} is only for a new hold).
func TestHandlePauseReplaceOmittedScopeKeepsPrevious(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"providers":["gamma.example"],"duration":"1h"}`)))
	if rr.Code != 200 {
		t.Fatalf("add = %d %s", rr.Code, rr.Body.Bytes())
	}
	st := pauseJSON(t, rr)
	h0 := st["holds"].([]any)[0].(map[string]any)
	id := h0["id"].(string)
	until := h0["until"]
	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"id":"`+id+`"}`)))
	if rr.Code != 200 {
		t.Fatalf("replace omitted scope = %d %s", rr.Code, rr.Body.Bytes())
	}
	st = pauseJSON(t, rr)
	h := st["holds"].([]any)[0].(map[string]any)
	if h["all"] != false || h["until"] != until {
		t.Fatalf("omitted-scope replace mutated hold %v", h)
	}
	if p.scheduler.HoldsTarget("c", "beta.example") || !p.scheduler.HoldsTarget("c", "gamma.example") {
		t.Fatalf("omitted-scope replace became %+v", st["holds"])
	}
}

func TestHandlePauseReplaceUnknown404(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"id":"missing","clients":["c"]}`)))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHandlePauseReplaceOverlap409(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	for _, body := range []string{
		`{"paused":true,"clients":["a"]}`,
		`{"paused":true,"clients":["b"]}`,
	} {
		rr := httptest.NewRecorder()
		p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause", strings.NewReader(body)))
		if rr.Code != 200 {
			t.Fatalf("add = %d %s", rr.Code, rr.Body.Bytes())
		}
	}
	var idA string
	for _, h := range p.PauseSnapshot()["holds"].([]map[string]any) {
		for _, c := range h["clients"].([]string) {
			if c == "a" {
				idA = h["id"].(string)
			}
		}
	}
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"id":"`+idA+`","clients":["b"]}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	if !p.scheduler.Holds("a") || !p.scheduler.Holds("b") {
		t.Fatal("failed replace must leave both holds")
	}
}
