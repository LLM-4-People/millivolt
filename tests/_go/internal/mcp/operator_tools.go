package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestOperatorStateReadsFourSurfaces pins the one-call read: pause, limits,
// quota/storm and restart status, each an authenticated GET.
func TestOperatorStateReadsFourSurfaces(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, pausePath, `{"ok":true,"paused":true,"holds":[{"id":"h1","all":true,"queued":2}],"known_clients":[],"known_providers":[]}`)
	proxy.json(http.MethodGet, throttlePath, `{"ok":true,"active":true,"throttles":[{"provider":"local","concurrency":2}],"known_providers":["local","other"]}`)
	proxy.json(http.MethodGet, quotaPath, `{"ok":true,"quota_gates":[{"provider":"local"}]}`)
	proxy.json(http.MethodGet, "/admin/restart", `{"ok":true,"phase":"idle","available":true}`)

	service := newTestService(t, proxy, Limits{})
	out, err := service.operatorState(context.Background(), OperatorStateInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Pause["paused"] != true || out.Throttle["active"] != true || out.Quota["ok"] != true || out.Restart["available"] != true {
		t.Fatalf("operator state = %+v", out)
	}
	if len(out.Problems) != 0 {
		t.Fatalf("no surface failed: %v", out.Problems)
	}
	for _, path := range []string{pausePath, throttlePath, quotaPath, "/admin/restart"} {
		calls := proxy.requestsFor(http.MethodGet, path)
		if len(calls) != 1 {
			t.Fatalf("expected one GET %s, got %d", path, len(calls))
		}
		assertAuth(t, calls[0])
	}
	if len(proxy.requestsFor(http.MethodPost, pausePath)) != 0 {
		t.Fatal("reading state must never mutate it")
	}
}

// TestOperatorStateReportsPartialFailure pins that one unreachable surface is
// reported while the rest still answer, and that a total failure is an error.
func TestOperatorStateReportsPartialFailure(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, pausePath, `{"ok":true,"paused":false}`)
	proxy.fail(http.MethodGet, throttlePath, http.StatusServiceUnavailable, "unavailable", "")
	proxy.json(http.MethodGet, quotaPath, `{"ok":true}`)
	proxy.json(http.MethodGet, "/admin/restart", `{"ok":true}`)

	service := newTestService(t, proxy, Limits{})
	out, err := service.operatorState(context.Background(), OperatorStateInput{})
	if err != nil {
		t.Fatalf("a single failed surface must not fail the read: %v", err)
	}
	if len(out.Problems) != 1 || !strings.Contains(out.Problems[0], "throttle") {
		t.Fatalf("problems = %v", out.Problems)
	}
	if out.Pause["paused"] != false || out.Quota["ok"] != true {
		t.Fatalf("the readable surfaces must still be filled: %+v", out)
	}

	// Every surface failing is a credential or connectivity problem, not four
	// independent ones.
	for _, path := range []string{pausePath, throttlePath, quotaPath, "/admin/restart"} {
		proxy.fail(http.MethodGet, path, http.StatusUnauthorized, "operator token required", "")
	}
	if _, err := service.operatorState(context.Background(), OperatorStateInput{}); err == nil {
		t.Fatal("a total failure must be an error")
	}
}

// TestSetPauseBodyShape pins that a resume sends only what the proxy needs, and
// that a hold carries its scope.
func TestSetPauseBodyShape(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodPost, pausePath, `{"ok":true,"paused":true,"holds":[{"id":"h1","all":true,"max_queued":4}],"warning":"State applied in memory, but could not be saved for restart."}`)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	limit := 4
	out, err := service.setPause(ctx, SetPauseInput{Paused: true, All: true, Duration: "1h", MaxQueued: &limit})
	if err != nil {
		t.Fatal(err)
	}
	request := proxy.requestsFor(http.MethodPost, pausePath)[0]
	assertAuth(t, request)
	body := decodeBody(t, request)
	if body["paused"] != true || body["all"] != true || body["duration"] != "1h" || body["max_queued"].(float64) != 4 {
		t.Fatalf("hold body = %v", body)
	}
	if _, present := body["clients"]; present {
		t.Fatalf("an unnamed scope must be omitted, not sent empty: %v", body)
	}
	if out.Scope != "all traffic" || !strings.Contains(out.Note, "never cancelled") {
		t.Fatalf("pause output = %+v", out)
	}
	if !strings.Contains(out.Warning, "could not be saved") {
		t.Fatalf("persistence warning must survive: %q", out.Warning)
	}

	resumed, err := service.setPause(ctx, SetPauseInput{Paused: false})
	if err != nil {
		t.Fatal(err)
	}
	resume := decodeBody(t, proxy.requestsFor(http.MethodPost, pausePath)[1])
	if len(resume) != 1 || resume["paused"] != false {
		t.Fatalf("a global resume must send only paused:false, got %v", resume)
	}
	if !resumed.Resumed || resumed.Scope != "resumed every hold" {
		t.Fatalf("resume output = %+v", resumed)
	}
}

// TestSetPauseValidation pins the local guards: a bad duration and a negative
// cap never reach the proxy.
func TestSetPauseValidation(t *testing.T) {
	proxy := newFakeProxy(t)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()
	negative := -1
	for _, tc := range []struct {
		name string
		in   SetPauseInput
		want string
	}{
		{"bad duration", SetPauseInput{Paused: true, All: true, Duration: "2m"}, "15m, 1h, 6h, 12h, 24h"},
		{"negative cap", SetPauseInput{Paused: true, All: true, MaxQueued: &negative}, "max_queued must be >= 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.setPause(ctx, tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
	if len(proxy.requests()) != 0 {
		t.Fatal("a refused hold must not reach the proxy")
	}
}

// TestSetThrottleBodyShapeAndClear pins the merge semantics and the clear.
func TestSetThrottleBodyShapeAndClear(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodPost, throttlePath, `{"ok":true,"active":true,"throttles":[{"provider":"local","concurrency":4}],`+
		`"known_providers":["local","other"]}`)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	concurrency := 4
	out, err := service.setThrottle(ctx, SetThrottleInput{Provider: "local", Concurrency: &concurrency})
	if err != nil {
		t.Fatal(err)
	}
	body := decodeBody(t, proxy.requestsFor(http.MethodPost, throttlePath)[0])
	if body["provider"] != "local" || body["concurrency"].(float64) != 4 {
		t.Fatalf("throttle body = %v", body)
	}
	for _, absent := range []string{"requests", "tokens", "request_window", "token_window", "clear"} {
		if _, present := body[absent]; present {
			t.Fatalf("an omitted dimension must be omitted so it keeps its value: %v", body)
		}
	}
	if len(out.KnownProviders) != 2 || !strings.Contains(out.Note, "not strict fixed-window") {
		t.Fatalf("throttle output = %+v", out)
	}

	if _, err := service.setThrottle(ctx, SetThrottleInput{Provider: "local", Clear: true}); err != nil {
		t.Fatal(err)
	}
	cleared := decodeBody(t, proxy.requestsFor(http.MethodPost, throttlePath)[1])
	if cleared["clear"] != true || len(cleared) != 2 {
		t.Fatalf("clear body = %v", cleared)
	}

	for _, in := range []SetThrottleInput{
		{},
		{Provider: "  "},
		{Provider: "local", Concurrency: throttleIntPtr(-2)},
	} {
		if _, err := service.setThrottle(ctx, in); err == nil {
			t.Fatalf("set_throttle(%+v) must be refused", in)
		}
	}
}

// TestResumeQuotaSendsLiteralResume pins that resume is always the literal true
// the proxy requires, and that the output separates the retry-mode gate from a
// manual hold.
func TestResumeQuotaSendsLiteralResume(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodPost, quotaPath, `{"ok":true,"quota_gates":[]}`)
	service := newTestService(t, proxy, Limits{})
	out, err := service.resumeQuota(context.Background(), ResumeQuotaInput{Provider: "local"})
	if err != nil {
		t.Fatal(err)
	}
	body := decodeBody(t, proxy.requestsFor(http.MethodPost, quotaPath)[0])
	if body["provider"] != "local" || body["resume"] != true {
		t.Fatalf("resume body = %v", body)
	}
	if !out.Resumed {
		t.Fatal("no gate remains, so the provider was resumed")
	}
	if !strings.Contains(out.Note, "manual-mode quota hold resumes through the pause surface") {
		t.Fatalf("note = %q", out.Note)
	}
	if _, err := service.resumeQuota(context.Background(), ResumeQuotaInput{}); err == nil {
		t.Fatal("a missing provider must be refused")
	}
}

// TestSetConfigIsARevisionCheckedPatch pins the read-then-patch behaviour, the
// exact body, and the saved-but-reload-failed report.
func TestSetConfigIsARevisionCheckedPatch(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, configPath, `{"revision":"abc123","values":{"history_size":10000}}`)
	proxy.json(http.MethodPost, configPath, `{"saved":true,"revision":"def456","values":{"history_size":2000},`+
		`"restart_required":["history_size"]}`)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	out, err := service.setConfig(ctx, SetConfigInput{Values: map[string]any{"history_size": 2000}})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Saved || len(out.RestartRequired) != 1 || out.RestartRequired[0] != "history_size" {
		t.Fatalf("config output = %+v", out)
	}
	body := decodeBody(t, proxy.requestsFor(http.MethodPost, configPath)[0])
	if body["revision"] != "abc123" {
		t.Fatalf("an empty revision must use the one just read: %v", body)
	}
	values, ok := body["values"].(map[string]any)
	if !ok || len(values) != 1 || values["history_size"].(float64) != 2000 {
		t.Fatalf("values must carry only the changed keys: %v", body["values"])
	}

	// An explicit revision is used verbatim, with no read.
	if _, err := service.setConfig(ctx, SetConfigInput{Values: map[string]any{"history_size": 3}, Revision: "mine"}); err != nil {
		t.Fatal(err)
	}
	explicit := decodeBody(t, proxy.requestsFor(http.MethodPost, configPath)[1])
	if explicit["revision"] != "mine" {
		t.Fatalf("explicit revision = %v", explicit["revision"])
	}

	// A stale revision surfaces the proxy's 409 rather than overwriting.
	proxy.fail(http.MethodPost, configPath, http.StatusConflict,
		"settings changed since they were loaded; revert to reload before applying", "")
	if _, err := service.setConfig(ctx, SetConfigInput{Values: map[string]any{"history_size": 4}, Revision: "stale"}); err == nil ||
		!strings.Contains(err.Error(), "409") {
		t.Fatalf("a stale revision must surface as an error, got %v", err)
	}

	// Saved but reload failed: the file changed, and the output must say so.
	proxy.json(http.MethodPost, configPath, `{"saved":true,"values":{},"error":"reload failed: bad key"}`)
	saved, err := service.setConfig(ctx, SetConfigInput{Values: map[string]any{"history_size": 5}, Revision: "mine"})
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Saved || saved.Error == "" || !strings.Contains(saved.Warning, "file was written") {
		t.Fatalf("saved-but-reload-failed must be explicit: %+v", saved)
	}

	if _, err := service.setConfig(ctx, SetConfigInput{}); err == nil {
		t.Fatal("an empty patch must be refused")
	}
}

// TestReloadConfig pins the one route the dashboard never calls.
func TestReloadConfig(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodPost, reloadPath, `{"ok":true,"restart_required":["db_path"]}`)
	service := newTestService(t, proxy, Limits{})
	out, err := service.reloadConfig(context.Background(), ReloadConfigInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !out.OK || len(out.RestartRequired) != 1 {
		t.Fatalf("reload output = %+v", out)
	}
	assertAuth(t, proxy.requestsFor(http.MethodPost, reloadPath)[0])
}

// TestPurgePreviewRefusesAnEmptyFilter pins that the delete-everything command
// has no path through this server at all.
func TestPurgePreviewRefusesAnEmptyFilter(t *testing.T) {
	proxy := newFakeProxy(t)
	service := newTestService(t, proxy, Limits{})
	if _, err := service.purgePreview(context.Background(), PurgePreviewInput{}); err == nil ||
		!strings.Contains(err.Error(), "never previews or sends the delete-everything command") {
		t.Fatalf("error = %v, want the empty-filter refusal", err)
	}
	if len(proxy.requests()) != 0 {
		t.Fatal("no preview may reach the proxy for an empty filter")
	}
}

// TestPurgeConfirmationGuards is the destructive-call property: the deletion is
// impossible without all three guards, and no empty body is ever sent.
func TestPurgeConfirmationGuards(t *testing.T) {
	filter := PurgeFilterInput{Provider: "local"}
	proxy := newFakeProxy(t)
	proxy.json(http.MethodPost, purgeCountPat, `{"count":4}`)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	// No confirmation, whatever else is right.
	if _, err := service.purge(ctx, PurgeInput{Filter: filter, ReviewedCount: 4}); err == nil ||
		!strings.Contains(err.Error(), "confirmation must be the exact phrase") {
		t.Fatalf("a missing confirmation must refuse, got %v", err)
	}
	// A plausible but wrong phrase is not enough either.
	for _, phrase := range []string{"yes", "delete", "purge", "permanently delete the matching millivolt history "} {
		if _, err := service.purge(ctx, PurgeInput{Filter: filter, Confirmation: phrase, ReviewedCount: 4}); err == nil {
			t.Fatalf("the confirmation %q must not be accepted", phrase)
		}
	}
	// Right phrase, wrong reviewed count.
	for _, count := range []int64{0, 3, 5, -1} {
		if _, err := service.purge(ctx, PurgeInput{
			Filter: filter, Confirmation: PurgeConfirmation, ReviewedCount: count,
		}); err == nil {
			t.Fatalf("reviewed_count %d must not be accepted", count)
		}
	}
	// Right phrase and count, but no filter: still refused, and still no
	// bodyless request.
	if _, err := service.purge(ctx, PurgeInput{
		Filter: PurgeFilterInput{}, Confirmation: PurgeConfirmation, ReviewedCount: 4,
	}); err == nil || !strings.Contains(err.Error(), "deletes ALL history") {
		t.Fatalf("an empty filter must be refused, got %v", err)
	}
	if len(proxy.requestsFor(http.MethodPost, purgePath)) != 0 {
		t.Fatal("no refused purge may reach the proxy")
	}
	if len(proxy.requests()) == 0 {
		t.Fatal("the count re-check must actually run, so a stale review is caught")
	}
	for _, request := range proxy.requests() {
		if strings.TrimSpace(request.Body) == "" && request.Method == http.MethodPost {
			t.Fatalf("a bodyless POST would be the delete-everything command: %s %s", request.Method, request.Path)
		}
	}
}

// TestPurgeSendsTheExactReviewedFilter pins the happy path and the filter wire
// shape: only constraining fields are sent, and zero values are omitted so the
// proxy reads them as "no constraint" rather than a match on 0.
func TestPurgeSendsTheExactReviewedFilter(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodPost, purgeCountPat, `{"count":4}`)
	proxy.json(http.MethodPost, purgePath, `{"ok":true}`)
	service := newTestService(t, proxy, Limits{})

	out, err := service.purge(context.Background(), PurgeInput{
		Filter:        PurgeFilterInput{Client: "dev", HasError: true, StatusCode: 500, BeforeMs: 1000},
		Confirmation:  PurgeConfirmation,
		ReviewedCount: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Deleted || out.Recount != 4 || out.Reviewed != 4 {
		t.Fatalf("purge output = %+v", out)
	}
	if !strings.Contains(out.Irreversible, "cannot be recovered") {
		t.Fatalf("the irreversibility must be in the output: %q", out.Irreversible)
	}
	requests := proxy.requestsFor(http.MethodPost, purgePath)
	if len(requests) != 1 {
		t.Fatalf("expected exactly one deletion, got %d", len(requests))
	}
	assertAuth(t, requests[0])
	body := decodeBody(t, requests[0])
	if body["client"] != "dev" || body["has_error"] != true || body["status_code"].(float64) != 500 || body["before_ms"].(float64) != 1000 {
		t.Fatalf("purge body = %v", body)
	}
	for _, absent := range []string{"provider", "model", "conversation_id", "error_type", "debug", "after_ms"} {
		if _, present := body[absent]; present {
			t.Fatalf("an unset constraint must be omitted, but %q was sent: %v", absent, body)
		}
	}
	// The pre-delete and post-delete counts both used the same filter.
	counts := proxy.requestsFor(http.MethodPost, purgeCountPat)
	if len(counts) != 2 {
		t.Fatalf("expected the count before and after, got %d", len(counts))
	}
	if counts[0].Body != requests[0].Body || counts[1].Body != requests[0].Body {
		t.Fatalf("every count must use the deleted filter: %q %q vs %q", counts[0].Body, counts[1].Body, requests[0].Body)
	}
}

// TestPurgeFilterRoundTripsEveryField pins the wire shape for the whole
// predicate, so a new field cannot be added to the tool and silently dropped.
func TestPurgeFilterWireShape(t *testing.T) {
	full := PurgeFilterInput{
		Provider: "p", Model: "m", Client: "c", ConversationID: "cv", ErrorType: "e",
		StatusCode: 429, HasError: true, HasDebug: true, BeforeMs: 2000, AfterMs: 1000,
	}
	document := full.document()
	if len(document) != 10 {
		t.Fatalf("every field must reach the proxy: %v", document)
	}
	if document["debug"] != true {
		t.Fatalf("the debug filter is named debug on the wire: %v", document)
	}
	// Zero values are absence, not a constraint.
	empty := (PurgeFilterInput{}).document()
	if len(empty) != 0 {
		t.Fatalf("an empty filter must serialize to nothing, got %v", empty)
	}
	if !(PurgeFilterInput{}).isEmpty() {
		t.Fatal("an empty filter must be recognized as the delete-everything command")
	}
	for _, partial := range []PurgeFilterInput{
		{Provider: "p"}, {Model: "m"}, {Client: "c"}, {ConversationID: "cv"},
		{ErrorType: "e"}, {StatusCode: 1}, {HasError: true}, {HasDebug: true},
		{BeforeMs: 1}, {AfterMs: 1},
	} {
		if partial.isEmpty() {
			t.Fatalf("%+v constrains something and must not read as empty", partial)
		}
	}
	if !strings.Contains(full.describe(), "client=c") {
		t.Fatalf("the human-legible description must name the constraints: %q", full.describe())
	}
}

// TestPurgePreviewOutput pins the preview mapping and the standing caveat.
func TestPurgePreviewOutput(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodPost, purgeCountPat, `{"count":17}`)
	service := newTestService(t, proxy, Limits{})
	out, err := service.purgePreview(context.Background(), PurgePreviewInput{
		Filter: PurgeFilterInput{Provider: "local", BeforeMs: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 17 || !out.Verified || !strings.Contains(out.Warning, "can change this count") {
		t.Fatalf("preview output = %+v", out)
	}
	if !strings.Contains(out.Filter, "provider=local") || !strings.Contains(out.Filter, "before_ms=100") {
		t.Fatalf("the previewed filter must be stated: %q", out.Filter)
	}
	assertAuth(t, proxy.requestsFor(http.MethodPost, purgeCountPat)[0])
}

func throttleIntPtr(value int) *int { return &value }
