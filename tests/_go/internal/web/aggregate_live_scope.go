package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestLiveStatusScopeOnFinalizedSurfaces pins the one rule for a live in-flight
// status class (s=streaming/paused/throttled/pending): the class names the
// pending registry, so a surface that reads only finalized contributions can
// never match it. The chart must answer invalid scope rather than a zeroed
// bucket grid that reads as "no traffic" for a filter that is impossible
// there; the durable log page keeps its established empty page (the client
// stops paging instead of showing "history unavailable"); the explorer folds
// pending rows and therefore keeps matching the class. The durable spellings
// stay accepted on all three.
func TestLiveStatusScopeOnFinalizedSurfaces(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	s.Record(mkRec("durable-ok", now.Add(-time.Minute), 200, "", nil, 10, 100, 1, 1, 0, 0))
	s.Record(mkRec("durable-err", now.Add(-2*time.Minute), 505, "boom", nil, 10, 100, 1, 1, 0, 0))
	waitTotals(t, s, 2)
	buf := metrics.NewBuffer(16)
	paused := mkRec("live-paused", now, 0, "", nil, 0, 0, 0, 0, 0, 0)
	paused.Stream, paused.Paused = true, true
	buf.PublishLive("begin", paused)
	api := NewAggAPI(buf, s, time.Second)

	routes := []struct {
		name   string
		handle http.Handler
		target func(scope string) string
	}{
		{"chart", http.HandlerFunc(api.HandleAggChart), func(scope string) string {
			return "/metrics/agg/chart?window=all&" + scope
		}},
		{"log", http.HandlerFunc(api.HandleLogPage), func(scope string) string {
			return "/metrics/agg/log?limit=10&" + scope
		}},
		{"explorer", http.HandlerFunc(api.HandleAggExplorer), func(scope string) string {
			return "/metrics/agg/explorer?dim=provider&" + scope
		}},
	}
	do := func(t *testing.T, h http.Handler, target string) (int, []byte) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec.Code, rec.Body.Bytes()
	}

	// Every live class is covered through the owner slice, so adding a class
	// cannot leave a surface unguarded.
	if len(liveStatusClasses) == 0 {
		t.Fatal("liveStatusClasses is empty: the live-class contract has no owner")
	}
	for _, class := range liveStatusClasses {
		for _, route := range routes {
			t.Run(class+"/"+route.name, func(t *testing.T) {
				code, body := do(t, route.handle, route.target("s="+class))
				switch route.name {
				case "chart":
					var flat struct {
						Error string `json:"error"`
					}
					if err := json.Unmarshal(body, &flat); err != nil {
						t.Fatalf("chart body is not the flat error shape: %v (%q)", err, body)
					}
					if code != http.StatusBadRequest || flat.Error != "invalid scope" {
						t.Fatalf("chart %s -> %d %q, want the log path's 400 invalid scope", class, code, flat.Error)
					}
				case "log":
					var page struct {
						Records []any `json:"records"`
						More    bool  `json:"more"`
					}
					if err := json.Unmarshal(body, &page); err != nil {
						t.Fatalf("log page unparseable: %v (%q)", err, body)
					}
					if code != http.StatusOK || len(page.Records) != 0 || page.More {
						t.Fatalf("log %s -> %d with %d records (more=%v), want the standing empty page", class, code, len(page.Records), page.More)
					}
				case "explorer":
					var payload struct {
						Scope struct {
							Matches int64 `json:"matches"`
						} `json:"scope"`
					}
					if err := json.Unmarshal(body, &payload); err != nil {
						t.Fatalf("explorer payload unparseable: %v (%q)", err, body)
					}
					want := int64(0)
					if class == "paused" {
						want = 1 // the pending row above is paused
					}
					if code != http.StatusOK || payload.Scope.Matches != want {
						t.Fatalf("explorer %s -> %d matches=%d, want 200 matches=%d (pending rows fold here)",
							class, code, payload.Scope.Matches, want)
					}
				}
			})
		}
	}

	// Durable selectors stay accepted everywhere: an exact code and the
	// explorer's own class grammar.
	for _, route := range routes {
		t.Run("durable/"+route.name, func(t *testing.T) {
			if code, body := do(t, route.handle, route.target("s=200")); code != http.StatusOK {
				t.Fatalf("%s s=200 -> %d, want 200 (%s)", route.name, code, body)
			}
		})
	}
	for _, route := range routes {
		t.Run("class-filter/"+route.name, func(t *testing.T) {
			if code, body := do(t, route.handle, route.target("f=status:2xx")); code != http.StatusOK {
				t.Fatalf("%s f=status:2xx -> %d, want 200 (%s)", route.name, code, body)
			}
		})
	}
	// The explorer's live status cards stay selectable: f=status:paused is a
	// gallery value, not the Requests-card s= selector, so the chart must keep
	// answering it (a refusal would strand the click in its retry state).
	for _, route := range routes {
		t.Run("live-component/"+route.name, func(t *testing.T) {
			code, body := do(t, route.handle, route.target("f=status:paused"))
			if code != http.StatusOK {
				t.Fatalf("%s f=status:paused -> %d, want 200 (%s)", route.name, code, body)
			}
		})
	}
}
