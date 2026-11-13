package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

func throttleDoc(t *testing.T, p *Server) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	p.HandleThrottle(rr, httptest.NewRequest(http.MethodGet, "/admin/throttle", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET throttle = %d %s", rr.Code, rr.Body.Bytes())
	}
	var doc map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func throttlePolicies(t *testing.T, p *Server) []map[string]any {
	t.Helper()
	doc := throttleDoc(t, p)
	raw, _ := doc["throttles"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		policy, _ := item.(map[string]any)
		out = append(out, policy)
	}
	return out
}

func policyScope(policy map[string]any) map[string]any {
	scope, _ := policy["scope"].(map[string]any)
	return scope
}

func scopeStrings(scope map[string]any, key string) []string {
	raw, _ := scope[key].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func TestHandleThrottleScopedRoundTrip(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := postThrottle(t, p, `{"models":["expensive-model"],"concurrency":2}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rr.Code, rr.Body.Bytes())
	}
	policies := throttlePolicies(t, p)
	if len(policies) != 1 {
		t.Fatalf("policies = %d, want 1", len(policies))
	}
	if got := scopeStrings(policyScope(policies[0]), "models"); !reflect.DeepEqual(got, []string{"expensive-model"}) {
		t.Fatalf("scope models = %v", got)
	}
	if policies[0]["concurrency"] != float64(2) {
		t.Fatalf("concurrency = %v", policies[0]["concurrency"])
	}
	id, _ := policies[0]["id"].(string)
	if id == "" {
		t.Fatal("policy has no id")
	}

	// An edit by id merges the omitted concurrency and applies the new window.
	rr = postThrottle(t, p, `{"id":"`+id+`","requests":10,"request_window":"1m"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("edit = %d %s", rr.Code, rr.Body.Bytes())
	}
	policies = throttlePolicies(t, p)
	if len(policies) != 1 || policies[0]["concurrency"] != float64(2) || policies[0]["requests"] != float64(10) {
		t.Fatalf("merged policy = %+v", policies)
	}

	rr = postThrottle(t, p, `{"id":"`+id+`","clear":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("clear = %d %s", rr.Code, rr.Body.Bytes())
	}
	if policies := throttlePolicies(t, p); len(policies) != 0 {
		t.Fatalf("policies after clear = %+v", policies)
	}
}

func TestHandleThrottleLegacyProviderShape(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	if rr := postThrottle(t, p, `{"provider":"neutral.example","concurrency":1}`); rr.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rr.Code, rr.Body.Bytes())
	}
	policies := throttlePolicies(t, p)
	if len(policies) != 1 {
		t.Fatalf("policies = %d", len(policies))
	}
	if policies[0]["provider"] != "neutral.example" {
		t.Fatalf("legacy provider = %v", policies[0]["provider"])
	}
	if got := scopeStrings(policyScope(policies[0]), "providers"); !reflect.DeepEqual(got, []string{"neutral.example"}) {
		t.Fatalf("scope providers = %v", got)
	}
}

func TestHandleThrottleScopeEditReplacesIdentity(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := postThrottle(t, p, `{"providers":["a.example"],"concurrency":1}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rr.Code, rr.Body.Bytes())
	}
	first := throttlePolicies(t, p)[0]
	oldID, _ := first["id"].(string)
	rr = postThrottle(t, p, `{"id":"`+oldID+`","providers":[],"models":["m1"],"concurrency":1}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("scope edit = %d %s", rr.Code, rr.Body.Bytes())
	}
	policies := throttlePolicies(t, p)
	if len(policies) != 1 {
		t.Fatalf("policies = %d, want the edit to replace one rule", len(policies))
	}
	if policies[0]["id"] == oldID {
		t.Fatal("scope edit kept the old policy identity")
	}
	if got := scopeStrings(policyScope(policies[0]), "models"); !reflect.DeepEqual(got, []string{"m1"}) {
		t.Fatalf("models = %v", got)
	}
	if got := scopeStrings(policyScope(policies[0]), "providers"); len(got) != 0 {
		t.Fatalf("providers survived a scope replacement: %v", got)
	}
}

func TestHandleThrottleConflictDoesNotReplaceNeighbour(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	if rr := postThrottle(t, p, `{"providers":["a.example"],"concurrency":1}`); rr.Code != http.StatusOK {
		t.Fatalf("seed a = %d", rr.Code)
	}
	if rr := postThrottle(t, p, `{"providers":["b.example"],"concurrency":2}`); rr.Code != http.StatusOK {
		t.Fatalf("seed b = %d", rr.Code)
	}
	policies := throttlePolicies(t, p)
	var aID string
	for _, policy := range policies {
		if got := scopeStrings(policyScope(policy), "providers"); len(got) == 1 && got[0] == "a.example" {
			aID, _ = policy["id"].(string)
		}
	}
	if aID == "" {
		t.Fatal("policy a not found")
	}
	rr := postThrottle(t, p, `{"id":"`+aID+`","providers":["b.example"],"concurrency":9}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("conflict = %d %s, want 409", rr.Code, rr.Body.Bytes())
	}
	policies = throttlePolicies(t, p)
	if len(policies) != 2 {
		t.Fatalf("conflict changed the policy set: %+v", policies)
	}
	for _, policy := range policies {
		got := scopeStrings(policyScope(policy), "providers")
		if len(got) == 1 && got[0] == "b.example" && policy["concurrency"] != float64(2) {
			t.Fatalf("conflict replaced the neighbour: %+v", policy)
		}
	}
}

func TestHandleThrottleRejectsAntiBurstOnScopedPolicy(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	for _, body := range []string{
		`{"models":["m"],"concurrency":2,"anti_burst":true,"ramp_duration":"10s"}`,
		`{"providers":["a.example"],"days":[1],"concurrency":2,"anti_burst":true,"ramp_duration":"10s"}`,
	} {
		rr := postThrottle(t, p, body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s, want 400", body, rr.Code, rr.Body.Bytes())
		}
	}
	if policies := throttlePolicies(t, p); len(policies) != 0 {
		t.Fatalf("rejected anti-burst stored a policy: %+v", policies)
	}
}

func TestHandleThrottleRejectsInvalidScopeAndSchedule(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	for _, body := range []string{
		`{"key_hashes":["short"],"concurrency":1}`,
		`{"providers":["a"],"days":[7],"concurrency":1}`,
		`{"providers":["a"],"days":[1,1],"concurrency":1}`,
		`{"providers":["a"],"start_minute":1440,"concurrency":1}`,
		`{"providers":["a"],"end_minute":-1,"concurrency":1}`,
		`{"provider":"a","providers":["b"],"concurrency":1}`,
		`{"concurrency":1}`,
	} {
		rr := postThrottle(t, p, body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s, want 400", body, rr.Code, rr.Body.Bytes())
		}
	}
}

func TestHandleThrottleScheduleRoundTrip(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	body := `{"providers":["a.example"],"days":[1,3],"start_minute":540,"end_minute":1020,"concurrency":1}`
	if rr := postThrottle(t, p, body); rr.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rr.Code, rr.Body.Bytes())
	}
	policies := throttlePolicies(t, p)
	schedule, _ := policies[0]["schedule"].(map[string]any)
	if schedule == nil {
		t.Fatalf("schedule missing: %+v", policies[0])
	}
	if schedule["start_minute"] != float64(540) || schedule["end_minute"] != float64(1020) {
		t.Fatalf("schedule = %+v", schedule)
	}
	days, _ := schedule["days"].([]any)
	if len(days) != 2 || days[0] != float64(1) || days[1] != float64(3) {
		t.Fatalf("schedule days = %v", days)
	}
}

func TestPersistedScopedPolicyRoundTrip(t *testing.T) {
	store := &countingThrottlePersist{}
	p := New(config.Default(), metrics.Noop{})
	p.attachThrottlePersist(store)
	deferThrottleWriter(p)
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })

	scope := scheduler.LimitScope{
		Providers:     []string{"a.example"},
		Models:        []string{"m1", "m2"},
		Clients:       []string{"agent-a"},
		KeyHashes:     []string{strings.Repeat("a", 64)},
		Conversations: []string{"conv-1"},
	}
	schedule := scheduler.LimitSchedule{Days: []int{1, 2}, StartMin: 60, EndMin: 120}
	policy := scheduler.Throttle{
		Scope:     scope,
		Schedule:  schedule,
		Limit:     scheduler.Limit{Concurrency: 3, Requests: 5, ReqWindow: time.Minute},
		Source:    scheduler.ThrottleSourceUI,
		UpdatedBy: "dashboard",
		UpdatedAt: time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC),
	}
	if err := applyTestThrottle(p, policy); err != nil {
		t.Fatal(err)
	}
	_, raw, _, _ := store.stats()
	if len(raw) == 0 {
		t.Fatal("no document was written")
	}
	if !strings.Contains(string(raw), `"version":2`) {
		t.Fatalf("document is not version 2: %s", raw)
	}

	restored := New(config.Default(), metrics.Noop{})
	restored.attachThrottlePersist(store)
	policies := throttlePolicies(t, restored)
	if len(policies) != 1 {
		t.Fatalf("restored policies = %d", len(policies))
	}
	if !reflect.DeepEqual(scopeStrings(policyScope(policies[0]), "models"), []string{"m1", "m2"}) {
		t.Fatalf("restored models = %v", scopeStrings(policyScope(policies[0]), "models"))
	}
	if !reflect.DeepEqual(scopeStrings(policyScope(policies[0]), "clients"), []string{"agent-a"}) {
		t.Fatalf("restored clients = %v", scopeStrings(policyScope(policies[0]), "clients"))
	}
	scheduleDoc, _ := policies[0]["schedule"].(map[string]any)
	if scheduleDoc == nil || scheduleDoc["start_minute"] != float64(60) {
		t.Fatalf("restored schedule = %+v", scheduleDoc)
	}
}
