package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/proxy"
)

func stringPtr(value string) *string { return &value }

// proxyServerTransport dispatches the operator routes these regressions exercise
// to a real proxy handler. Bootstrap carries the storage signal the tools gate
// on, so the calls run the production HTTP boundary rather than a canned body.
type proxyServerTransport struct{ p *proxy.Server }

func (t *proxyServerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	switch req.URL.Path {
	case "/admin/debug":
		t.p.HandleDebug(rec, req)
	case "/admin/pause":
		t.p.HandlePause(rec, req)
	case throttlePath:
		t.p.HandleThrottle(rec, req)
	case "/metrics/bootstrap":
		rec.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(rec, `{"seq":1,"feed_id":"f","counters":{},"storage":{"enabled":true},"records":[]}`)
	default:
		http.NotFound(rec, req)
	}
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

func newProxyBackedService(t *testing.T) (*Service, *proxy.Server) {
	t.Helper()
	store, _ := openModelStorage(t)
	p := proxy.New(config.Default(), metrics.Noop{})
	p.AttachPausePersist(store)
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })
	service, err := NewServiceWithTransport(InProcessOrigin, testToken, defaultTestLimits(),
		&proxyServerTransport{p: p})
	if err != nil {
		t.Fatal(err)
	}
	return service, p
}

// TestAuditStartPartialEditPreservesOmittedScope pins MCP-1 at the proxy
// boundary: naming only clients must not clear providers, while an explicit
// empty array still clears that one dimension.
func TestAuditStartPartialEditPreservesOmittedScope(t *testing.T) {
	service, p := newProxyBackedService(t)
	ctx := context.Background()
	created, err := service.auditStart(ctx, AuditStartInput{
		Providers: []string{"local"}, Confirm: auditConfirmStart,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Started.Providers) != 1 || created.Started.Providers[0] != "local" {
		t.Fatalf("create scope = %+v", created.Started)
	}
	edited, err := service.auditStart(ctx, AuditStartInput{
		SessionID: created.Started.ID, Clients: []string{"dev"}, Confirm: auditConfirmStart,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(edited.Started.Clients) != 1 || edited.Started.Clients[0] != "dev" {
		t.Fatalf("edit clients = %v", edited.Started.Clients)
	}
	if len(edited.Started.Providers) != 1 || edited.Started.Providers[0] != "local" {
		t.Fatalf("omitted providers were dropped by a partial edit: %v", edited.Started.Providers)
	}
	sessions, _ := p.DebugSnapshot()["sessions"].([]map[string]any)
	if len(sessions) != 1 {
		t.Fatalf("proxy sessions = %v", sessions)
	}
	if providers, _ := sessions[0]["providers"].([]string); len(providers) != 1 || providers[0] != "local" {
		t.Fatalf("proxy state lost the omitted provider scope: %v", sessions[0])
	}

	// Explicit empty clears one dimension; the other still follows presence.
	cleared, err := service.auditStart(ctx, AuditStartInput{
		SessionID: created.Started.ID, Providers: []string{}, Confirm: auditConfirmStart,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared.Started.Providers) != 0 || len(cleared.Started.Clients) != 1 {
		t.Fatalf("explicit empty providers must clear only that dimension: %+v", cleared.Started)
	}

	// Restating both dimensions keeps both, the control for the partial edit.
	both, err := service.auditStart(ctx, AuditStartInput{
		SessionID: created.Started.ID, Clients: []string{"dev"}, Providers: []string{"local"},
		Confirm: auditConfirmStart,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(both.Started.Clients) != 1 || len(both.Started.Providers) != 1 {
		t.Fatalf("both-dims restatement = %+v", both.Started)
	}
}

// TestAuditStartDurationPresenceClearsAndPreserves pins MCP-2: an explicit
// empty duration clears the deadline; an omitted one preserves it.
func TestAuditStartDurationPresenceClearsAndPreserves(t *testing.T) {
	service, _ := newProxyBackedService(t)
	ctx := context.Background()
	created, err := service.auditStart(ctx, AuditStartInput{
		Clients: []string{"dur"}, Duration: stringPtr("15m"), Confirm: auditConfirmStart,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Started.Until == nil {
		t.Fatal("create with a duration carries no deadline")
	}
	preserved, err := service.auditStart(ctx, AuditStartInput{
		SessionID: created.Started.ID, Clients: []string{"dur"}, Confirm: auditConfirmStart,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Started.Until == nil || *preserved.Started.Until != *created.Started.Until {
		t.Fatalf("omitted duration changed the deadline: %v -> %v", created.Started.Until, preserved.Started.Until)
	}
	cleared, err := service.auditStart(ctx, AuditStartInput{
		SessionID: created.Started.ID, Duration: stringPtr(""), Confirm: auditConfirmStart,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Started.Until != nil {
		t.Fatalf("explicit empty duration did not clear the deadline: %v", cleared.Started.Until)
	}
}

func pausedHoldID(t *testing.T, state map[string]any) string {
	t.Helper()
	holds, _ := state["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("holds = %v", state["holds"])
	}
	hold, _ := holds[0].(map[string]any)
	id, _ := hold["id"].(string)
	if id == "" {
		t.Fatalf("hold has no id: %v", hold)
	}
	return id
}

func pausedHold(t *testing.T, state map[string]any, id string) map[string]any {
	t.Helper()
	holds, _ := state["holds"].([]any)
	for _, raw := range holds {
		hold, _ := raw.(map[string]any)
		if hold["id"] == id {
			return hold
		}
	}
	t.Fatalf("hold %q missing from %v", id, state["holds"])
	return nil
}

func pausedHoldNames(t *testing.T, hold map[string]any, field string) []string {
	t.Helper()
	raw, ok := hold[field]
	if !ok || raw == nil {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("%s = %T %v, want an array", field, raw, raw)
	}
	out := make([]string, 0, len(list))
	for _, value := range list {
		name, _ := value.(string)
		out = append(out, name)
	}
	return out
}

func pausedHoldUntil(t *testing.T, state map[string]any, id string) any {
	t.Helper()
	holds, _ := state["holds"].([]any)
	for _, raw := range holds {
		hold, _ := raw.(map[string]any)
		if hold["id"] == id {
			return hold["until"]
		}
	}
	t.Fatalf("hold %q missing from %v", id, state["holds"])
	return nil
}

// TestSetPauseDurationPresenceClearsAndPreserves is the pause twin of MCP-2.
func TestSetPauseDurationPresenceClearsAndPreserves(t *testing.T) {
	service, _ := newProxyBackedService(t)
	ctx := context.Background()
	created, err := service.setPause(ctx, SetPauseInput{
		Paused: true, All: true, Duration: stringPtr("15m"),
	})
	if err != nil {
		t.Fatal(err)
	}
	id := pausedHoldID(t, created.State)
	if until := pausedHoldUntil(t, created.State, id); until == nil {
		t.Fatal("create with a duration carries no deadline")
	}
	preserved, err := service.setPause(ctx, SetPauseInput{Paused: true, HoldID: id})
	if err != nil {
		t.Fatal(err)
	}
	if until := pausedHoldUntil(t, preserved.State, id); until == nil {
		t.Fatal("omitted duration cleared the hold deadline")
	}
	cleared, err := service.setPause(ctx, SetPauseInput{Paused: true, HoldID: id, Duration: stringPtr("")})
	if err != nil {
		t.Fatal(err)
	}
	if until := pausedHoldUntil(t, cleared.State, id); until != nil {
		t.Fatalf("explicit empty duration did not clear the hold deadline: %v", until)
	}
}

// TestSetPausePartialEditPreservesOmittedScope pins MCP-4 at the tool boundary:
// editing a hold by naming only clients keeps its providers, and an explicit
// empty providers array remains the way to clear that dimension.
func TestSetPausePartialEditPreservesOmittedScope(t *testing.T) {
	service, _ := newProxyBackedService(t)
	ctx := context.Background()
	created, err := service.setPause(ctx, SetPauseInput{Paused: true, Providers: []string{"alpha.example"}})
	if err != nil {
		t.Fatal(err)
	}
	id := pausedHoldID(t, created.State)

	edited, err := service.setPause(ctx, SetPauseInput{Paused: true, HoldID: id, Clients: []string{"client-a"}})
	if err != nil {
		t.Fatal(err)
	}
	hold := pausedHold(t, edited.State, id)
	if names := pausedHoldNames(t, hold, "providers"); len(names) != 1 || names[0] != "alpha.example" {
		t.Fatalf("client-only edit dropped providers: %v", hold)
	}
	if names := pausedHoldNames(t, hold, "clients"); len(names) != 1 || names[0] != "client-a" {
		t.Fatalf("edited clients = %v", hold)
	}
	if want := "edited hold " + id + ": clients client-a and providers alpha.example"; edited.Scope != want {
		t.Fatalf("client-only edit scope = %q, want %q", edited.Scope, want)
	}

	preserved, err := service.setPause(ctx, SetPauseInput{Paused: true, HoldID: id})
	if err != nil {
		t.Fatal(err)
	}
	hold = pausedHold(t, preserved.State, id)
	if names := pausedHoldNames(t, hold, "providers"); len(names) != 1 || names[0] != "alpha.example" {
		t.Fatalf("no-scope edit dropped providers: %v", hold)
	}
	if names := pausedHoldNames(t, hold, "clients"); len(names) != 1 || names[0] != "client-a" {
		t.Fatalf("no-scope edit dropped clients: %v", hold)
	}
	if want := "edited hold " + id + ": clients client-a and providers alpha.example"; preserved.Scope != want {
		t.Fatalf("no-scope edit scope = %q, want %q", preserved.Scope, want)
	}

	cleared, err := service.setPause(ctx, SetPauseInput{Paused: true, HoldID: id, Providers: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	hold = pausedHold(t, cleared.State, id)
	if names := pausedHoldNames(t, hold, "providers"); len(names) != 0 {
		t.Fatalf("explicit empty providers did not clear: %v", hold)
	}
	if names := pausedHoldNames(t, hold, "clients"); len(names) != 1 || names[0] != "client-a" {
		t.Fatalf("empty-provider edit dropped clients: %v", hold)
	}
	if want := "edited hold " + id + ": clients client-a"; cleared.Scope != want {
		t.Fatalf("empty-provider edit scope = %q, want %q", cleared.Scope, want)
	}
	if strings.Contains(cleared.Scope, "alpha.example") {
		t.Fatalf("cleared scope still names the provider: %q", cleared.Scope)
	}
}

// createBarrierTransport holds both concurrent creates until each has reached
// the proxy, so the test exercises the overlapping-snapshot case deterministically.
type createBarrierTransport struct {
	base    http.RoundTripper
	posts   atomic.Int32
	release chan struct{}
	once    sync.Once
}

func (t *createBarrierTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && req.URL.Path == "/admin/debug" {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		var document map[string]any
		_ = json.Unmarshal(body, &document)
		if _, hasID := document["id"]; !hasID {
			if t.posts.Add(1) == 2 {
				t.once.Do(func() { close(t.release) })
			}
			<-t.release
		}
	}
	return t.base.RoundTrip(req)
}

// TestConcurrentAuditCreatesReturnTheirOwnIDs pins MCP-3: the create response
// carries the authoritative installed id, so two overlapping creates cannot
// both resolve to the same first-new session.
func TestConcurrentAuditCreatesReturnTheirOwnIDs(t *testing.T) {
	store, _ := openModelStorage(t)
	p := proxy.New(config.Default(), metrics.Noop{})
	p.AttachPausePersist(store)
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })
	barrier := &createBarrierTransport{base: &proxyServerTransport{p: p}, release: make(chan struct{})}
	service, err := NewServiceWithTransport(InProcessOrigin, testToken, defaultTestLimits(), barrier)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	type result struct {
		out *AuditStartOutput
		err error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i, client := range []string{"a", "b"} {
		wg.Add(1)
		go func(i int, client string) {
			defer wg.Done()
			results[i].out, results[i].err = service.auditStart(ctx, AuditStartInput{
				Clients: []string{client}, Confirm: auditConfirmStart,
			})
		}(i, client)
	}
	wg.Wait()
	for i, result := range results {
		if result.err != nil {
			t.Fatalf("create %d: %v", i, result.err)
		}
	}
	first, second := results[0].out.Started.ID, results[1].out.Started.ID
	if first == "" || second == "" || first == second {
		t.Fatalf("concurrent ids = %q and %q", first, second)
	}
	sessions, _ := p.DebugSnapshot()["sessions"].([]map[string]any)
	if len(sessions) != 2 {
		t.Fatalf("proxy sessions after concurrent creates = %v", sessions)
	}
	for _, id := range []string{first, second} {
		if _, err := service.auditStop(ctx, AuditStopInput{SessionID: id}); err != nil {
			t.Fatalf("stop %q: %v", id, err)
		}
	}
	if sessions, _ := p.DebugSnapshot()["sessions"].([]map[string]any); len(sessions) != 0 {
		t.Fatalf("stops left live sessions: %v", sessions)
	}
}

// TestAuditStopUnknownSessionDoesNotReportSuccess pins MCP-8: the proxy answers
// not-found, so the tool cannot report a stable-looking success while the real
// capture keeps running.
func TestAuditStopUnknownSessionDoesNotReportSuccess(t *testing.T) {
	service, p := newProxyBackedService(t)
	ctx := context.Background()
	created, err := service.auditStart(ctx, AuditStartInput{
		Clients: []string{"live"}, Confirm: auditConfirmStart,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.auditStop(ctx, AuditStopInput{SessionID: "missing-session"}); err == nil {
		t.Fatal("unknown session stop reported success")
	}
	sessions, _ := p.DebugSnapshot()["sessions"].([]map[string]any)
	if len(sessions) != 1 || sessions[0]["id"] != created.Started.ID {
		t.Fatalf("unknown stop changed the live session set: %v", sessions)
	}
	stopped, err := service.auditStop(ctx, AuditStopInput{SessionID: created.Started.ID})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Stopped != created.Started.ID {
		t.Fatalf("real stop reported %q", stopped.Stopped)
	}
	if sessions, _ := p.DebugSnapshot()["sessions"].([]map[string]any); len(sessions) != 0 {
		t.Fatalf("real stop left a live session: %v", sessions)
	}
}

// TestSetPauseDescriptionsDistinguishCreateAndEdit pins MCP-4's model-facing
// wording: only a create treats an omitted scope as global; an edit preserves.
func TestSetPauseDescriptionsDistinguishCreateAndEdit(t *testing.T) {
	service := newTestService(t, newFakeProxy(t), Limits{})
	session := connect(t, service)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var description, schema string
	for _, tool := range listed.Tools {
		if tool.Name != "set_pause" {
			continue
		}
		description = tool.Description
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		schema = string(raw)
	}
	if description == "" || schema == "" {
		t.Fatal("set_pause is not registered")
	}
	if strings.Contains(schema, "rejects a hold with no scope at all") {
		t.Fatalf("schema still claims a scopeless hold is rejected: %s", schema)
	}
	for _, needle := range []string{"on a CREATE", "on an EDIT", "preserve"} {
		if !strings.Contains(strings.ToLower(schema), strings.ToLower(needle)) {
			t.Fatalf("schema does not distinguish create/edit (%q missing): %s", needle, schema)
		}
	}
	if !strings.Contains(description, "On an EDIT") || !strings.Contains(description, "preserve") {
		t.Fatalf("description does not distinguish create/edit: %s", description)
	}
}

// TestDescribeDescriptionStatesToolNamesNoValue pins the describe tool's
// top-level wording to the canonical 'null' no-value representation.
func TestDescribeDescriptionStatesToolNamesNoValue(t *testing.T) {
	service := newTestService(t, newFakeProxy(t), Limits{})
	session := connect(t, service)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name != "describe" {
			continue
		}
		if strings.Contains(tool.Description, "JSON-array text columns") {
			t.Fatalf("describe still claims JSON-array text columns: %s", tool.Description)
		}
		if !strings.Contains(tool.Description, "attempts/tool_names are JSON text columns holding an array, or 'null' when there is no value") {
			t.Fatalf("describe does not state the canonical no-value document: %s", tool.Description)
		}
		return
	}
	t.Fatal("describe is not registered")
}

// TestDescribeNoteKeepsModelFiltersGoOwned pins the INT-1 residual: the
// request-log note lists only the SQL-exact log-page equalities and states that
// model matching remains Go-owned.
func TestDescribeNoteKeepsModelFiltersGoOwned(t *testing.T) {
	var note string
	for _, table := range tableDocs {
		if table.Table == "requests" {
			note = strings.Join(table.Notes, " ")
			break
		}
	}
	if note == "" {
		t.Fatal("requests table notes missing")
	}
	if strings.Contains(note, "conversation_id, model, status_code") {
		t.Fatalf("note still claims model pushdown: %s", note)
	}
	for _, needle := range []string{"provider", "client", "key_hash", "conversation_id", "status_code"} {
		if !strings.Contains(note, needle) {
			t.Fatalf("note omits pushed equality %q: %s", needle, note)
		}
	}
	if !strings.Contains(note, "Model filters are Go-owned") {
		t.Fatalf("note does not state model filters are Go-owned: %s", note)
	}
}

// TestValuesToolToleratesEmptyAndMalformedToolNames pins MCP-5: a migrated
// empty tool_names column is no vocabulary, never a malformed-JSON 500.
func TestValuesToolToleratesEmptyAndMalformedToolNames(t *testing.T) {
	store, path := openModelStorage(t)
	seedModelSpellings(t, path, []string{"m1"})
	proxy := newStorageModelProxy(t, store)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stored := range []string{"", "{bad"} {
		if _, err := db.Exec(`UPDATE requests SET tool_names = ?`, stored); err != nil {
			t.Fatal(err)
		}
		out, err := service.values(ctx, ValuesInput{Dim: "tool"})
		if err != nil {
			t.Fatalf("tool_names=%q produced an error: %v", stored, err)
		}
		if out.Returned != 0 || len(out.Values) != 0 {
			t.Fatalf("tool_names=%q produced values: %+v", stored, out.Values)
		}
	}
	if _, err := db.Exec(`UPDATE requests SET tool_names = '["search"]'`); err != nil {
		t.Fatal(err)
	}
	out, err := service.values(ctx, ValuesInput{Dim: "tool"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Returned != 1 || out.Values[0].Value != "search" || out.Values[0].Requests != 1 {
		t.Fatalf("valid array enumeration = %+v", out.Values)
	}
}

// TestValuesToolCountsDuplicateNamesOncePerRequest pins MCP-6: one request that
// carries a name twice contributes one request, not two occurrences.
func TestValuesToolCountsDuplicateNamesOncePerRequest(t *testing.T) {
	store, path := openModelStorage(t)
	seedModelSpellings(t, path, []string{"m1", "m2"})
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE requests SET tool_names = '["search","search"]' WHERE id = 'model-value-00000'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE requests SET tool_names = '["other"]' WHERE id = 'model-value-00001'`); err != nil {
		t.Fatal(err)
	}
	proxy := newStorageModelProxy(t, store)
	service := newTestService(t, proxy, Limits{})
	out, err := service.values(context.Background(), ValuesInput{Dim: "tool"})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, value := range out.Values {
		counts[value.Value] = value.Requests
	}
	if counts["search"] != 1 || counts["other"] != 1 {
		t.Fatalf("duplicate-name counts = %v, want search=1 other=1", counts)
	}
}

// TestScopeStatusIsCanonicalized pins MCP-9: every accepted numeric spelling is
// forwarded in the canonical form the proxy matcher compares.
func TestScopeStatusIsCanonicalized(t *testing.T) {
	for _, status := range []string{"200", "0200", "+200"} {
		scope := Scope{Status: status}
		if err := scope.Validate(); err != nil {
			t.Fatalf("status %q rejected: %v", status, err)
		}
		if got := scope.query().Get("s"); got != "200" {
			t.Fatalf("status %q forwarded as %q, want 200", status, got)
		}
	}
	for _, tool := range []string{"streaming", "paused", "throttled", "pending"} {
		if got := (Scope{Status: tool}).query().Get("s"); got != tool {
			t.Fatalf("live class %q forwarded as %q", tool, got)
		}
	}
	if err := (Scope{Status: "many"}).Validate(); err == nil {
		t.Fatal("non-numeric, non-class status accepted")
	}
}
