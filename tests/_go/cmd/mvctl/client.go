package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

// TestOperatorClientCarriesTheQueryBudget pins the client contract: a
// full-history fold through GetJSONQuery must wait for a slow answer. The
// command client used to be built from an empty Limits set, and a zero
// query timeout expires the derived context before the request leaves.
func TestOperatorClientCarriesTheQueryBudget(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/metrics/agg/chart": func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"series":[]}`))
		},
	})
	client, err := newOperatorClient(Global{BaseURL: fixture.url(), Token: testToken, Settings: defaultSettings()})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := client.GetJSONQuery(t.Context(), mcp.RouteChart, nil, &out); err != nil {
		t.Fatalf("GetJSONQuery = %v, want the slow answer inside the query budget", err)
	}
	if len(fixture.callsFor("/metrics/agg/chart")) != 1 {
		t.Fatal("the chart request never reached the proxy")
	}
}
