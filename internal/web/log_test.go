package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestLogPageRetainsTimestampTies(t *testing.T) {
	s := testStore(t)
	start := time.UnixMilli(1_800_000_000_000)
	for i := range 31 {
		s.Record(&metrics.Record{ID: fmt.Sprintf("tie-%02d", i), Provider: "p", Start: start, StatusCode: 200})
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(NewAggAPI(metrics.NewBuffer(2), s, time.Second).HandleLogPage)
	first := get(t, h, fmt.Sprintf("/metrics/agg/log?before_ms=%d&before_id=&limit=10", start.UnixMilli()+1))
	if len(first["records"].([]any)) != 10 {
		t.Fatalf("first page: %v", first)
	}
	second := get(t, h, fmt.Sprintf("/metrics/agg/log?before_ms=%d&before_id=tie-21&limit=10", start.UnixMilli()))
	rows := second["records"].([]any)
	if len(rows) != 10 || rows[0].(map[string]any)["id"] != "tie-20" || rows[9].(map[string]any)["id"] != "tie-11" {
		t.Fatalf("timestamp tie lost at page boundary: %v", second)
	}
}

func TestLogPageScopeBudgetAdvancesAcrossTimestampTies(t *testing.T) {
	s := testStore(t)
	start := time.UnixMilli(1_800_000_000_000)
	for i := range logScanMax + 7 {
		provider := "other"
		if i < 7 {
			provider = "target"
		}
		s.Record(&metrics.Record{ID: fmt.Sprintf("row-%04d", i), Provider: provider, Start: start, StatusCode: 200})
		if i%logScanBatch == 0 {
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(NewAggAPI(metrics.NewBuffer(1), s, time.Second).HandleLogPage)
	first := get(t, h, "/metrics/agg/log?limit=10&f=provider:target")
	if len(first["records"].([]any)) != 0 || first["more"] != true || first["cursor_id"] != "row-0007" {
		t.Fatalf("scope-budget cursor=%v", first)
	}
	second := get(t, h, fmt.Sprintf("/metrics/agg/log?limit=10&f=provider:target&before_ms=%.0f&before_id=%s", first["cursor_ms"], url.QueryEscape(first["cursor_id"].(string))))
	if len(second["records"].([]any)) != 7 || second["more"] != false {
		t.Fatalf("scope-budget continuation lost rows=%v", second)
	}
	for _, target := range []string{
		"?limit=10&before_ms=1", "?limit=10&before_id=x", "?limit=10&before_ms=nope&before_id=x",
		"?limit=10&before_ms=1&before_ms=2&before_id=x", "?limit=10&before_ms=1&before_id=x&before_id=y",
		"?limit=10&limit=20",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics/agg/log"+target, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("malformed cursor accepted: %s status=%d", target, w.Code)
		}
	}
}

func TestLogPageStartsAtDurableNewestWithoutRing(t *testing.T) {
	s := testStore(t)
	s.Record(&metrics.Record{ID: "durable", Provider: "p", Start: time.UnixMilli(1_800_000_000_000), StatusCode: 200})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	api := NewAggAPI(metrics.NewBuffer(2), s, time.Second)
	w := httptest.NewRecorder()
	api.HandleLogPage(w, httptest.NewRequest(http.MethodGet, "/metrics/agg/log?limit=10", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("cursorless first archive page: status=%d body=%s", w.Code, w.Body.String())
	}
	page := get(t, http.HandlerFunc(api.HandleLogPage), "/metrics/agg/log?limit=10")
	if rows := page["records"].([]any); len(rows) != 1 || rows[0].(map[string]any)["id"] != "durable" {
		t.Fatalf("empty/late-start ring must not hide durable history: %v", page)
	}
}
