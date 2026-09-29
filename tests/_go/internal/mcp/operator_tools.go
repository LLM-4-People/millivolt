package mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestOperatorStateReadsFourSurfaces pins the one-call read: pause, limits,
// quota/storm and restart status, each an authenticated GET.
func TestOperatorStateReadsFourSurfaces(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, pausePath, `{"ok":true,"paused":true,"holds":[{"id":"h1","all":true,"queued":2}],"known_clients":[],"known_providers":[]}`)
	proxy.json(http.MethodGet, throttlePath, `{"ok":true,"active":true,"throttles":[{"provider":"local","concurrency":2}],"known_providers":["local","other"]}`)
	proxy.json(http.MethodGet, quotaPath, `{"enabled":true,"banner_enabled":true,"storms":[`+
		`{"provider":"local","model":"m","scope":"provider","state":"parked","quota":true,"error_percent":1}]}`)
	proxy.json(http.MethodGet, "/admin/restart", `{"ok":true,"phase":"idle","available":true}`)

	service := newTestService(t, proxy, Limits{})
	out, err := service.operatorState(context.Background(), OperatorStateInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Pause["paused"] != true || out.Throttle["active"] != true ||
		out.Quota["enabled"] != true || out.Restart["available"] != true {
		t.Fatalf("operator state = %+v", out)
	}
	if len(out.Problems) != 0 {
		t.Fatalf("no surface failed: %v", out.Problems)
	}
	for _, path := range []string{pausePath, throttlePath, quotaPath, restartPath} {
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
	proxy.json(http.MethodGet, restartPath, `{"ok":true}`)

	service := newTestService(t, proxy, Limits{})
	out, err := service.operatorState(context.Background(), OperatorStateInput{})
	if err != nil {
		t.Fatalf("a single failed surface must not fail the read: %v", err)
	}
	if len(out.Problems) != 1 || !strings.Contains(out.Problems[0], "throttle") {
		t.Fatalf("problems = %v", out.Problems)
	}
	if out.Pause["paused"] != false || out.Quota["ok"] != true { // the partial-failure fixture keeps ok
		t.Fatalf("the readable surfaces must still be filled: %+v", out)
	}

	// Every surface failing is a credential or connectivity problem, not four
	// independent ones.
	for _, path := range []string{pausePath, throttlePath, quotaPath, restartPath} {
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
	proxy.json(http.MethodGet, debugPath, emptyDebugStatus)
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

// TestSetThrottleAppliesTheProxysOwnBands pins the bands the proxy enforces in
// internal/proxy/throttle.go. Checking only ">= 0" let a request through that
// the proxy would reject after assembling it, and a window on its own was
// accepted by both and set nothing while answering 200.
func TestSetThrottleAppliesTheProxysOwnBands(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, debugPath, emptyDebugStatus)
	// The provider has no counts yet, so a window-only body would set nothing.
	proxy.json(http.MethodGet, throttlePath, `{"ok":true,"throttles":[{"provider":"local","requests":0,"tokens":0}],`+
		`"known_providers":["local"]}`)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		in   SetThrottleInput
		want string
	}{
		{"concurrency above the ceiling", SetThrottleInput{Provider: "local", Concurrency: throttleIntPtr(100_001)}, "concurrency must be 0..100000"},
		{"requests above the ceiling", SetThrottleInput{Provider: "local", Requests: throttleInt64Ptr(1_000_000_001)}, "requests must be 0..1000000000"},
		// The exact message, not a substring: "tokens must be 0..1000000000"
		// is a prefix of the real token ceiling AND the whole requests message,
		// so a substring check passed when the token band was accidentally
		// narrowed to the request band.
		{"tokens above the ceiling", SetThrottleInput{Provider: "local", Tokens: throttleInt64Ptr(1_000_000_000_001)}, "tokens must be 0..1000000000000"},
		{"negative requests", SetThrottleInput{Provider: "local", Requests: throttleInt64Ptr(-1)}, "requests must be 0..1000000000"},
		{"window below the band", SetThrottleInput{Provider: "local", Requests: throttleInt64Ptr(5), RequestWindow: "500ms"}, `request_window: window must be 1s..24h0m0s, got "500ms"`},
		{"window above the band", SetThrottleInput{Provider: "local", Tokens: throttleInt64Ptr(5), TokenWindow: "48h"}, `token_window: window must be 1s..24h0m0s, got "48h"`},
		{"day form above the band", SetThrottleInput{Provider: "local", Tokens: throttleInt64Ptr(5), TokenWindow: "2d"}, `token_window: window must be 1s..24h0m0s, got "2d"`},
		{"unparsable window", SetThrottleInput{Provider: "local", Requests: throttleInt64Ptr(5), RequestWindow: "soon"}, `request_window: invalid window "soon"`},
		{"empty window", SetThrottleInput{Provider: "local", Requests: throttleInt64Ptr(5), RequestWindow: " "}, "request_window: window required"},
		{
			"window with no count and no existing count", SetThrottleInput{Provider: "local", RequestWindow: "1m"},
			`request_window: provider "local" has no request count yet, so a window alone would set nothing; send requests too`,
		},
		{
			"token window with no count and no existing count", SetThrottleInput{Provider: "local", TokenWindow: "1h"},
			`token_window: provider "local" has no token count yet, so a window alone would set nothing; send tokens too`,
		},
		{
			"a body that names no dimension", SetThrottleInput{Provider: "local"},
			"send concurrency, requests or tokens: a body that names no dimension sets nothing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.setThrottle(ctx, tc.in); err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want exactly %q", err, tc.want)
			}
		})
	}
	if len(proxy.requestsFor(http.MethodPost, throttlePath)) != 0 {
		t.Fatal("a request the proxy would refuse must not be sent at all")
	}
	// A window-only change is accepted when the dimension already has a count:
	// the proxy merges it into the existing policy.
	proxy.json(http.MethodGet, throttlePath, `{"ok":true,"throttles":[{"provider":"local","requests":5,"tokens":7}],`+
		`"known_providers":["local"]}`)
	proxy.json(http.MethodPost, throttlePath, `{"ok":true,"throttles":[],"known_providers":["local"]}`)
	for _, in := range []SetThrottleInput{
		{Provider: "local", RequestWindow: "1m"},
		{Provider: "local", TokenWindow: "2h"},
	} {
		if _, err := service.setThrottle(ctx, in); err != nil {
			t.Fatalf("%+v merges into an existing count and must be accepted: %v", in, err)
		}
	}
	// The bands' own edges are accepted, including the 1s and 24h window ends and
	// the day form the proxy accepts.
	for _, in := range []SetThrottleInput{
		{Provider: "local", Concurrency: throttleIntPtr(100_000)},
		{Provider: "local", Requests: throttleInt64Ptr(1_000_000_000), RequestWindow: "1s"},
		{Provider: "local", Tokens: throttleInt64Ptr(1_000_000_000_000), TokenWindow: "24h"},
		{Provider: "local", Requests: throttleInt64Ptr(1), RequestWindow: "1d"},
		{Provider: "local", Requests: throttleInt64Ptr(1), RequestWindow: "90m"},
		{Provider: "local", Concurrency: throttleIntPtr(0)},
	} {
		proxy.json(http.MethodPost, throttlePath, `{"ok":true,"throttles":[],"known_providers":["local"]}`)
		if _, err := service.setThrottle(ctx, in); err != nil {
			t.Fatalf("%+v is inside the proxy's bands and must be accepted: %v", in, err)
		}
	}
}

// TestSetThrottleDescriptionStatesThePinnedBands pins the model-visible
// set_throttle description to the MCP-local band constants. Those constants are
// the copies the proxy suite's TestMCPThrottleSchemaMirrorsTheProxyBands pins to
// internal/proxy/throttle.go, and the expected text here is derived from them,
// so a hand-written description that keeps the old numbers fails when a band
// changes. The registered tool is checked too, so wiring a literal description
// in server.go instead of setThrottleDescription fails as well.
func TestSetThrottleDescriptionStatesThePinnedBands(t *testing.T) {
	bands := []string{
		fmt.Sprintf("concurrency (0..%d)", maxLimitConcurrency),
		fmt.Sprintf("requests per window (0..%d)", maxLimitRequests),
		fmt.Sprintf("tokens per window (0..%d)", maxLimitTokens),
		fmt.Sprintf("A window is %s to %dh", minLimitWindow, int(maxLimitWindow/time.Hour)),
	}
	for _, band := range bands {
		if !strings.Contains(setThrottleDescription, band) {
			t.Fatalf("the set_throttle description must state %q: %q", band, setThrottleDescription)
		}
	}
	session := connect(t, newTestService(t, newFakeProxy(t), Limits{}))
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name != "set_throttle" {
			continue
		}
		if tool.Description != setThrottleDescription {
			t.Fatalf("the registered set_throttle description must be the owner var, got %q", tool.Description)
		}
		return
	}
	t.Fatal("the set_throttle tool is not registered")
}

// TestSetThrottleSchemaTagsStateThePinnedBands parses each band token out of
// the SetThrottleInput jsonschema tags and compares the whole token to the text
// derived from the MCP-local constants. A substring check is not enough: a tag
// widened from "0 to 100000" to "0 to 1000000" still contains the old text, and
// an unanchored pattern also accepts the "0 to 100000" inside "10 to 100000",
// so bandPattern captures the complete band: each number must follow a
// non-digit and the digit run is consumed whole. The constants themselves are
// pinned to internal/proxy/throttle.go by the proxy suite's
// TestMCPThrottleSchemaMirrorsTheProxyBands.
func TestSetThrottleSchemaTagsStateThePinnedBands(t *testing.T) {
	bandPattern := regexp.MustCompile(`(?:^|[^0-9])([0-9]+s? to [0-9]+h?)`)
	window := fmt.Sprintf("%s to %dh", minLimitWindow, int(maxLimitWindow/time.Hour))
	for _, tc := range []struct {
		field string
		want  string
	}{
		{"Concurrency", fmt.Sprintf("0 to %d", maxLimitConcurrency)},
		{"Requests", fmt.Sprintf("0 to %d", maxLimitRequests)},
		{"Tokens", fmt.Sprintf("0 to %d", maxLimitTokens)},
		{"RequestWindow", window},
		{"TokenWindow", window},
	} {
		field, ok := reflect.TypeOf(SetThrottleInput{}).FieldByName(tc.field)
		if !ok {
			t.Fatalf("SetThrottleInput has no %s field", tc.field)
		}
		tag := string(field.Tag.Get("jsonschema"))
		match := bandPattern.FindStringSubmatch(tag)
		if match == nil {
			t.Fatalf("SetThrottleInput.%s schema states no band: %q", tc.field, tag)
		}
		if got := match[1]; got != tc.want {
			t.Fatalf("SetThrottleInput.%s schema states the band %q, want %q (owner: the maxLimit*/minLimitWindow constants): %q",
				tc.field, got, tc.want, tag)
		}
	}
}

// TestSetThrottleWindowOnlyBodyDoesNotInventACount pins the wire shape of the
// merge: the window is sent, and no count the caller did not name is added, so
// the proxy applies the new window to the stored count.
func TestSetThrottleWindowOnlyBodyDoesNotInventACount(t *testing.T) {
	const existing = `{"ok":true,"throttles":[{"provider":"local","requests":5,"tokens":9}],"known_providers":["local"]}`
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, debugPath, emptyDebugStatus)
	proxy.json(http.MethodGet, throttlePath, existing)
	proxy.json(http.MethodPost, throttlePath, existing)
	service := newTestService(t, proxy, Limits{})
	if _, err := service.setThrottle(context.Background(), SetThrottleInput{Provider: "local", RequestWindow: "1m"}); err != nil {
		t.Fatal(err)
	}
	requests := proxy.requestsFor(http.MethodGet, throttlePath)
	if len(requests) != 1 {
		t.Fatalf("a window-only change must read the current policy once, got %d", len(requests))
	}
	assertAuth(t, requests[0])
	body := decodeBody(t, proxy.requestsFor(http.MethodPost, throttlePath)[0])
	if body["request_window"] != "1m" {
		t.Fatalf("the window must be sent: %v", body)
	}
	for _, absent := range []string{"requests", "tokens", "concurrency"} {
		if _, present := body[absent]; present {
			t.Fatalf("a window-only change must not invent %q: %v", absent, body)
		}
	}
}

// TestSetThrottleRefusesAnUnknownProvider pins the vocabulary guard. The proxy
// records the provider name on any accepted throttle write, so a typo does not
// fail - it adds a name that every later validation and discovery list is built
// from, permanently.
func TestSetThrottleRefusesAnUnknownProvider(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, debugPath, emptyDebugStatus)
	proxy.json(http.MethodPost, throttlePath, `{"ok":true,"throttles":[],"known_providers":["local"]}`)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	for _, in := range []SetThrottleInput{
		{Provider: "locla", Concurrency: throttleIntPtr(2)},
		{Provider: "locla", Clear: true},
		// A window-only body must not turn the vocabulary refusal into the
		// misleading "no request count yet" message: the known-provider guard
		// runs before the stored-count read.
		{Provider: "locla", RequestWindow: "1m"},
	} {
		_, err := service.setThrottle(ctx, in)
		if err == nil {
			t.Fatalf("set_throttle(%+v) must refuse an unknown provider", in)
		}
		if !strings.Contains(err.Error(), "not one of the known providers") {
			t.Fatalf("error = %v, want the vocabulary refusal", err)
		}
		for _, needle := range []string{"locla", "permanently", "local"} {
			if !strings.Contains(err.Error(), needle) {
				t.Fatalf("the refusal must name the typo, the consequence and the known names: %q", err)
			}
		}
	}
	if len(proxy.requestsFor(http.MethodPost, throttlePath)) != 0 {
		t.Fatal("a refused provider must never reach the proxy")
	}
	if len(proxy.requestsFor(http.MethodGet, throttlePath)) != 0 {
		t.Fatal("a refused provider must not read the current policy")
	}
	// A known provider is accepted; a clear is checked too, since the proxy
	// records the name on that path as well.
	if _, err := service.setThrottle(ctx, SetThrottleInput{Provider: "local", Concurrency: throttleIntPtr(2)}); err != nil {
		t.Fatalf("a known provider must be accepted: %v", err)
	}
	// A proxy that has never seen a provider has an empty vocabulary; refusing
	// every name there would make the tool unusable, not safer.
	proxy.json(http.MethodGet, debugPath, `{"ok":true,"enabled":false,"sessions":[],"known_clients":null,`+
		`"known_providers":null,"known_models":null,"ttl":"168h","max_bytes":"1MiB"}`)
	if _, err := service.setThrottle(ctx, SetThrottleInput{Provider: "brand-new", Concurrency: throttleIntPtr(2)}); err != nil {
		t.Fatalf("an empty vocabulary must not refuse every name: %v", err)
	}
}

// TestResumeQuotaSendsLiteralResume pins that resume is always the literal true
// the proxy requires, and that the output separates the retry-mode gate from a
// manual hold.
func TestResumeQuotaSendsLiteralResume(t *testing.T) {
	proxy := newFakeProxy(t)
	// The real /admin/quota shape: a storm document whose entries carry the
	// open-gate flag. There is no top-level quota_gates list on this route.
	proxy.json(http.MethodPost, quotaPath, `{"enabled":true,"banner_enabled":true,"storms":`+
		`[{"provider":"other","quota":true},{"provider":"local","quota":false}]}`)
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
		t.Fatal("the local gate is closed, so the provider is resumed")
	}
	if len(out.Open) != 1 || out.Open[0] != "other" {
		t.Fatalf("open_gates = %v, want the provider that still has one", out.Open)
	}
	if !strings.Contains(out.Note, "manual-mode quota hold resumes through the pause surface") {
		t.Fatalf("note = %q", out.Note)
	}
	if _, err := service.resumeQuota(context.Background(), ResumeQuotaInput{}); err == nil {
		t.Fatal("a missing provider must be refused")
	}
}

// TestResumeQuotaReportsAnOpenGate pins the other half: a provider whose gate is
// still open is NOT resumed. It used to be unreachable, because the tool read a
// field the proxy never sent, so resumed was permanently true.
func TestResumeQuotaReportsAnOpenGate(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodPost, quotaPath, `{"enabled":true,"banner_enabled":true,`+
		`"storms":[{"provider":"local","state":"parked","quota":true}]}`)
	service := newTestService(t, proxy, Limits{})

	out, err := service.resumeQuota(context.Background(), ResumeQuotaInput{Provider: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Resumed {
		t.Fatal("a storm entry with quota true means the gate is still open: the provider is not resumed")
	}
	if len(out.Open) != 1 || out.Open[0] != "local" {
		t.Fatalf("open_gates = %v", out.Open)
	}
	// A storm with quota false, and a document with no storms at all, are both
	// closed - the flag is what decides, never the entry's presence.
	proxy.json(http.MethodPost, quotaPath, `{"enabled":true,"storms":[{"provider":"local","quota":false}]}`)
	closed, err := service.resumeQuota(context.Background(), ResumeQuotaInput{Provider: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if !closed.Resumed {
		t.Fatal("a storm entry without the quota flag is not an open gate")
	}
	proxy.json(http.MethodPost, quotaPath, `{"enabled":true,"storms":[]}`)
	empty, err := service.resumeQuota(context.Background(), ResumeQuotaInput{Provider: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Resumed || len(empty.Open) != 0 {
		t.Fatalf("no storms means no open gate: %+v", empty)
	}
}

// TestConfigGetCarriesTheProxyPinnedKeys pins the fields the proxy publishes
// and this tool used to drop. Without overrides a model cannot tell why a
// patched key did not change (set_config strips CLI-pinned keys), and the two
// canonical vocabularies are what a usage_keys or model-rules patch must name.
func TestConfigGetCarriesTheProxyPinnedKeys(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, configPath, `{"revision":"abc","values":{},"effective":{},"defaults":{},`+
		`"fields":[],"restart_required":[],"writable":true,`+
		`"overrides":{"listen":"127.0.0.1:8081","db_path":"/tmp/dev.db"},`+
		`"usage_fields":["input_tokens","output_tokens"],"model_fields":["reasoning_effort"]}`)
	service := newTestService(t, proxy, Limits{})
	out, err := service.configGet(context.Background(), ConfigGetInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Overrides["listen"] != "127.0.0.1:8081" || out.Overrides["db_path"] != "/tmp/dev.db" {
		t.Fatalf("overrides must survive the decode: %+v", out.Overrides)
	}
	if !slices.Equal(out.UsageFields, []string{"input_tokens", "output_tokens"}) {
		t.Fatalf("usage_fields = %v", out.UsageFields)
	}
	if !slices.Equal(out.ModelFields, []string{"reasoning_effort"}) {
		t.Fatalf("model_fields = %v", out.ModelFields)
	}
	if !strings.Contains(out.Note, "strips") {
		t.Fatalf("the note must explain that overrides are stripped from a patch: %q", out.Note)
	}
	assertAuth(t, proxy.requestsFor(http.MethodGet, configPath)[0])
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

	// Saved but reload failed. The proxy writes the file FIRST and only then
	// answers 500, so this branch is unreachable unless the 500 body is decoded
	// as a committed document: faking a 200 for it hid the defect.
	proxy.respond(http.MethodPost, configPath, cannedResponse{
		Status: http.StatusInternalServerError, ContentType: "application/json",
		Body: `{"saved":true,"revision":"zzz","values":{"history_size":5},` +
			`"restart_required":["history_size"],"error":"settings saved, but reload failed: bad key"}`,
	})
	saved, err := service.setConfig(ctx, SetConfigInput{Values: map[string]any{"history_size": 5}, Revision: "mine"})
	if err != nil {
		t.Fatalf("a committed mutation must not be reported as an error: %v", err)
	}
	if !saved.Saved || saved.Error == "" || !strings.Contains(saved.Warning, "file was written") {
		t.Fatalf("saved-but-reload-failed must be explicit: %+v", saved)
	}
	if len(saved.RestartRequired) != 1 || saved.RestartRequired[0] != "history_size" {
		t.Fatalf("the 500 body carries the restart set too: %+v", saved)
	}
	if saved.Values["history_size"] != float64(5) {
		t.Fatalf("the saved values must come through: %+v", saved.Values)
	}

	// A 500 that reports no save is an ordinary failure and stays an error, so
	// the tolerance cannot swallow a real rejection into a silent success.
	proxy.fail(http.MethodPost, configPath, http.StatusInternalServerError, "could not write the config file", "")
	if _, err := service.setConfig(ctx, SetConfigInput{Values: map[string]any{"history_size": 6}, Revision: "mine"}); err == nil ||
		!strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "could not write") {
		t.Fatalf("a 500 with no committed document must stay an error, got %v", err)
	}
	// The stale revision is still rejected: 409 is not the saved-but-not-reloaded
	// status, so a model is never told a rejected write succeeded.
	proxy.fail(http.MethodPost, configPath, http.StatusConflict, "settings changed since they were loaded", "")
	if _, err := service.setConfig(ctx, SetConfigInput{Values: map[string]any{"history_size": 7}, Revision: "stale"}); err == nil ||
		!strings.Contains(err.Error(), "409") {
		t.Fatalf("a stale revision must surface as an error, got %v", err)
	}

	if _, err := service.setConfig(ctx, SetConfigInput{}); err == nil {
		t.Fatal("an empty patch must be refused")
	}
}

// TestSetConfigRedactsTheCredentialOnTheToleratedPath pins the one path that
// decodes a response body itself. The committed 500 document is not built by
// newAPIError, so its .error text reached the model unredacted even though the
// proxy's reload failure can quote the request it just refused.
func TestSetConfigRedactsTheCredentialOnTheToleratedPath(t *testing.T) {
	reflected := "settings saved, but reload failed: upstream rejected Bearer " + testToken
	proxy := newFakeProxy(t)
	proxy.respond(http.MethodPost, configPath, cannedResponse{
		Status: http.StatusInternalServerError, ContentType: "application/json",
		Body: `{"saved":true,"revision":"z","values":{},"restart_required":[],"error":` + quote(reflected) + `}`,
	})
	service := newTestService(t, proxy, Limits{})
	out, err := service.setConfig(context.Background(), SetConfigInput{
		Values: map[string]any{"history_size": 5}, Revision: "mine",
	})
	if err != nil {
		t.Fatalf("a committed mutation must not be reported as an error: %v", err)
	}
	if out.Error == "" || out.Warning == "" {
		t.Fatalf("the committed failure must still be reported: %+v", out)
	}
	assertNoToken(t, "set_config error", out.Error)
	assertNoToken(t, "set_config warning", out.Warning)
	if !strings.Contains(out.Error, "[redacted]") {
		t.Fatalf("the redaction must be visible, not silent removal: %q", out.Error)
	}

	// The tolerated status whose body reports no save becomes an *APIError;
	// that message goes through the same redaction.
	proxy.respond(http.MethodPost, configPath, cannedResponse{
		Status: http.StatusInternalServerError, ContentType: "application/json",
		Body: `{"saved":false,"error":` + quote(reflected) + `}`,
	})
	_, err = service.setConfig(context.Background(), SetConfigInput{
		Values: map[string]any{"history_size": 6}, Revision: "mine",
	})
	if err == nil {
		t.Fatal("a 500 with no committed document must stay an error")
	}
	assertNoToken(t, "set_config APIError", err.Error())
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("the failure message must carry the redaction: %v", err)
	}
}

// TestResumeQuotaTrimsTheProviderOnce pins the trim. The proxy compares the
// provider verbatim, so a padded name used to reach a different gate than the
// response check: resume_quota(" local ") answered resumed=true while the real
// gate stayed open.
func TestResumeQuotaTrimsTheProviderOnce(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodPost, quotaPath, `{"enabled":true,"storms":[{"provider":"local","quota":true}]}`)
	service := newTestService(t, proxy, Limits{})

	out, err := service.resumeQuota(context.Background(), ResumeQuotaInput{Provider: " local "})
	if err != nil {
		t.Fatal(err)
	}
	body := decodeBody(t, proxy.requestsFor(http.MethodPost, quotaPath)[0])
	if body["provider"] != "local" {
		t.Fatalf("the body must carry the trimmed provider: %v", body)
	}
	if out.Resumed {
		t.Fatal("the gate for the trimmed name is still open, so the provider is not resumed")
	}
	if len(out.Open) != 1 || out.Open[0] != "local" {
		t.Fatalf("open_gates = %v", out.Open)
	}
	// A trimmed name with no open gate is the ordinary resumed answer.
	proxy.json(http.MethodPost, quotaPath, `{"enabled":true,"storms":[]}`)
	closed, err := service.resumeQuota(context.Background(), ResumeQuotaInput{Provider: "  local  "})
	if err != nil {
		t.Fatal(err)
	}
	if !closed.Resumed {
		t.Fatalf("a closed gate must report resumed: %+v", closed)
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

// purgeFixture wires the two count answers and the delete, and returns a service
// plus the token a preview for filter would issue. Every purge test needs a real
// token, because a tokenless purge must never reach the delete.
func purgeFixture(t *testing.T, count int64) (*Service, *fakeProxy, string) {
	t.Helper()
	proxy := newFakeProxy(t)
	proxy.json(http.MethodPost, purgeCountPat, `{"count":`+strconv.FormatInt(count, 10)+`}`)
	proxy.json(http.MethodPost, purgePath, `{"ok":true}`)
	service := newTestService(t, proxy, Limits{})
	preview, err := service.purgePreview(context.Background(), PurgePreviewInput{Filter: PurgeFilterInput{Provider: "local"}})
	if err != nil {
		t.Fatal(err)
	}
	return service, proxy, preview.PreviewToken
}

// TestPurgeConfirmationGuards is the destructive-call property: the deletion is
// impossible without all three guards, and no empty body is ever sent.
func TestPurgeConfirmationGuards(t *testing.T) {
	filter := PurgeFilterInput{Provider: "local"}
	service, proxy, token := purgeFixture(t, 4)
	ctx := context.Background()
	deleted := len(proxy.requestsFor(http.MethodPost, purgePath))

	// No confirmation, whatever else is right.
	if _, err := service.purge(ctx, PurgeInput{Filter: filter, PreviewToken: token}); err == nil ||
		!strings.Contains(err.Error(), "confirmation must be the exact phrase") {
		t.Fatalf("a missing confirmation must refuse, got %v", err)
	}
	// A plausible but wrong phrase is not enough either.
	for _, phrase := range []string{"yes", "delete", "purge", "permanently delete the matching millivolt history "} {
		if _, err := service.purge(ctx, PurgeInput{Filter: filter, Confirmation: phrase, PreviewToken: token}); err == nil {
			t.Fatalf("the confirmation %q must not be accepted", phrase)
		}
	}
	// The exact phrase but no preview token: the deletion is unauthorized.
	for _, missing := range []string{"", "not-a-token", "eyJhIjoxfQ", "....", "a.b"} {
		if _, err := service.purge(ctx, PurgeInput{
			Filter: filter, Confirmation: PurgeConfirmation, PreviewToken: missing,
		}); err == nil || !strings.Contains(err.Error(), "preview_token") {
			t.Fatalf("preview_token %q must not be accepted, got %v", missing, err)
		}
	}
	// Right phrase and token, but no filter: still refused, and still no
	// bodyless request.
	if _, err := service.purge(ctx, PurgeInput{
		Filter: PurgeFilterInput{}, Confirmation: PurgeConfirmation, PreviewToken: token,
	}); err == nil || !strings.Contains(err.Error(), "deletes ALL history") {
		t.Fatalf("an empty filter must be refused, got %v", err)
	}
	if got := len(proxy.requestsFor(http.MethodPost, purgePath)); got != deleted {
		t.Fatalf("no refused purge may reach the delete, saw %d", got-deleted)
	}
	for _, request := range proxy.requests() {
		if strings.TrimSpace(request.Body) == "" && request.Method == http.MethodPost {
			t.Fatalf("a bodyless POST would be the delete-everything command: %s %s", request.Method, request.Path)
		}
	}
}

// TestPurgeRefusesATokenForAnotherFilter is the bypass the count could not
// prevent: preview one filter, delete a DIFFERENT filter that happens to match
// the same number of rows. A bare count authorizes that; a filter-bound token
// must not.
func TestPurgeRefusesATokenForAnotherFilter(t *testing.T) {
	proxy := newFakeProxy(t)
	// Both filters match exactly 4 rows, so a count-only guard cannot tell them
	// apart. This is the whole reason the guard is a token.
	proxy.json(http.MethodPost, purgeCountPat, `{"count":4}`)
	proxy.json(http.MethodPost, purgePath, `{"ok":true}`)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	previewed := PurgeFilterInput{Provider: "local"}
	other := PurgeFilterInput{Client: "dev"}
	preview, err := service.purgePreview(ctx, PurgePreviewInput{Filter: previewed})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.purge(ctx, PurgeInput{
		Filter: other, Confirmation: PurgeConfirmation, PreviewToken: preview.PreviewToken,
	}); err == nil || !strings.Contains(err.Error(), "DIFFERENT filter") {
		t.Fatalf("a token for another filter must refuse, got %v", err)
	}
	if len(proxy.requestsFor(http.MethodPost, purgePath)) != 0 {
		t.Fatal("nothing may be deleted for a token that authorizes another filter")
	}

	// The same filter passes: the token is bound to the filter, not to the call.
	if _, err := service.purge(ctx, PurgeInput{
		Filter: previewed, Confirmation: PurgeConfirmation, PreviewToken: preview.PreviewToken,
	}); err != nil {
		t.Fatalf("the previewed filter must be deletable with its own token: %v", err)
	}
	if len(proxy.requestsFor(http.MethodPost, purgePath)) != 1 {
		t.Fatal("the authorized deletion must reach the proxy")
	}
}

// TestPurgeRefusesAnExpiredToken pins the expiry, without waiting for it: the
// token is issued against a clock the test controls.
func TestPurgeRefusesAnExpiredToken(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodPost, purgeCountPat, `{"count":4}`)
	proxy.json(http.MethodPost, purgePath, `{"ok":true}`)
	service := newTestService(t, proxy, Limits{})
	filter := PurgeFilterInput{Provider: "local"}

	issued := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	// A token is refused once its own lifetime has passed, and accepted before.
	later := issued.Add(previewTokenTTL - time.Second)
	if _, err := service.previewToken.verify(mustIssue(t, service, filter, 4, issued), filter, later); err != nil {
		t.Fatalf("a token inside its lifetime must verify: %v", err)
	}
	expired := issued.Add(previewTokenTTL)
	if _, err := service.previewToken.verify(mustIssue(t, service, filter, 4, issued), filter, expired); err == nil ||
		!strings.Contains(err.Error(), "expired") {
		t.Fatalf("a token at its expiry must be refused, got %v", err)
	}
	// And the whole path refuses one that is already expired, without a delete.
	stale, err := service.previewToken.issue(filter, 4, time.Now().Add(-previewTokenTTL-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.purge(context.Background(), PurgeInput{
		Filter: filter, Confirmation: PurgeConfirmation, PreviewToken: stale,
	}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("an expired token must refuse the deletion, got %v", err)
	}
	if len(proxy.requestsFor(http.MethodPost, purgePath)) != 0 {
		t.Fatal("an expired token must never reach the delete")
	}
}

func mustIssue(t *testing.T, service *Service, filter PurgeFilterInput, count int64, now time.Time) string {
	t.Helper()
	token, err := service.previewToken.issue(filter, count, now)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// TestPurgeRefusesATokenThatWasNeverIssued pins that the MAC is what authorizes
// the token. A well-formed payload is not self-authenticating: the count inside
// it proves nothing unless this server signed it with a key derived from the
// operator credential.
func TestPurgeRefusesATokenThatWasNeverIssued(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodPost, purgeCountPat, `{"count":4}`)
	proxy.json(http.MethodPost, purgePath, `{"ok":true}`)
	service := newTestService(t, proxy, Limits{})
	filter := PurgeFilterInput{Provider: "local"}

	real, err := service.previewToken.issue(filter, 4, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	payload, signature, _ := strings.Cut(real, ".")
	raw := mustDecode(t, payload)
	encode := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	// A payload whose count was raised from 4 to 4000, keeping the real
	// signature: the signature covers the count, so this cannot authorize more.
	counted := strings.Replace(string(raw), "\n1:4\n", "\n1:4000\n", 1)
	if counted == string(raw) {
		t.Fatalf("the payload layout changed; this test must follow it: %q", raw)
	}
	for name, forged := range map[string]string{
		"count edited under a real signature": encode([]byte(counted)) + "." + signature,
		"signature replaced":                  payload + "." + encode(make([]byte, 32)),
		"no signature at all":                 payload + ".",
		"signed by a different credential": payload + "." + encode(newPreviewToken(
			"a-completely-different-operator-token").sign(raw)),
		"garbage payload":   encode([]byte("not a preview token")) + "." + signature,
		"truncated payload": payload[:4] + "." + signature,
	} {
		if _, err := service.purge(context.Background(), PurgeInput{
			Filter: filter, Confirmation: PurgeConfirmation, PreviewToken: forged,
		}); err == nil {
			t.Fatalf("%s must be refused: %q", name, forged)
		}
	}
	if len(proxy.requestsFor(http.MethodPost, purgePath)) != 0 {
		t.Fatal("no forged token may reach the delete")
	}
	// The genuine one still works, so every refusal above was about the token.
	if _, err := service.purge(context.Background(), PurgeInput{
		Filter: filter, Confirmation: PurgeConfirmation, PreviewToken: real,
	}); err != nil {
		t.Fatalf("the genuine token must still authorize: %v", err)
	}
}

func mustDecode(t *testing.T, encoded string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestPurgeTokenIsStatelessAndBoundToOneCount pins the token's properties: it
// verifies with no server state, it carries the count it authorized, and the
// same filter issues a byte-identical token.
func TestPurgeTokenIsStatelessAndBoundToOneCount(t *testing.T) {
	service := newTestService(t, newFakeProxy(t), Limits{})
	filter := PurgeFilterInput{Provider: "local", HasError: true, BeforeMs: 2000, AfterMs: 1000}
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	first := mustIssue(t, service, filter, 7, now)
	if second := mustIssue(t, service, filter, 7, now); first != second {
		t.Fatalf("the same filter, count and clock must produce the same token: %q vs %q", first, second)
	}
	count, err := service.previewToken.verify(first, filter, now)
	if err != nil {
		t.Fatal(err)
	}
	if count != 7 {
		t.Fatalf("the token must carry the count it authorized, got %d", count)
	}
	// A different count is a different token, and neither authorizes the other.
	other := mustIssue(t, service, filter, 8, now)
	if other == first {
		t.Fatal("a different count must produce a different token")
	}
	if len(first) > 512 {
		t.Fatalf("a token must stay small enough to pass back as an argument: %d bytes", len(first))
	}
}

// TestPurgeRefusalDisclosesNoCount pins that a refusal is not a count oracle. A
// message carrying the live count lets a model call purge again and delete on the
// second attempt with no preview and no human in the loop.
func TestPurgeRefusalDisclosesNoCount(t *testing.T) {
	proxy := newFakeProxy(t)
	// The preview authorized 4 rows; by delete time the filter matches 3.
	proxy.json(http.MethodPost, purgeCountPat, `{"count":4}`)
	proxy.json(http.MethodPost, purgePath, `{"ok":true}`)
	service := newTestService(t, proxy, Limits{})
	filter := PurgeFilterInput{Provider: "local"}
	preview, err := service.purgePreview(context.Background(), PurgePreviewInput{Filter: filter})
	if err != nil {
		t.Fatal(err)
	}
	proxy.json(http.MethodPost, purgeCountPat, `{"count":3}`)

	_, err = service.purge(context.Background(), PurgeInput{
		Filter: filter, Confirmation: PurgeConfirmation, PreviewToken: preview.PreviewToken,
	})
	if err == nil {
		t.Fatal("a moved row set must refuse")
	}
	if !strings.Contains(err.Error(), "purge_preview") {
		t.Fatalf("the refusal must say what to do next: %v", err)
	}
	if strings.Contains(err.Error(), "3 rows") || strings.Contains(err.Error(), "(3)") {
		t.Fatalf("the refusal leaks the live count, which is a count oracle: %q", err)
	}
	if len(proxy.requestsFor(http.MethodPost, purgePath)) != 0 {
		t.Fatal("a refused purge must not delete")
	}
}

// TestPurgeFilterValidationMatchesTheProxy pins the proxy's own purge-filter
// rules client-side, so a model gets a corrective message instead of a raw 400
// for a value the proxy was always going to reject.
func TestPurgeFilterValidationMatchesTheProxy(t *testing.T) {
	proxy := newFakeProxy(t)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		filter PurgeFilterInput
		want   string
	}{
		{"status above the band", PurgeFilterInput{Provider: "p", StatusCode: 1000}, "status_code must be 0..999"},
		{"negative status", PurgeFilterInput{Provider: "p", StatusCode: -1}, "status_code must be 0..999"},
		{"negative after", PurgeFilterInput{Provider: "p", AfterMs: -1}, "0 or greater"},
		{"negative before", PurgeFilterInput{Provider: "p", BeforeMs: -1}, "0 or greater"},
		{"window inverted", PurgeFilterInput{Provider: "p", BeforeMs: 1000, AfterMs: 2000}, "after_ms must be strictly less than before_ms"},
		{"window equal", PurgeFilterInput{Provider: "p", BeforeMs: 1000, AfterMs: 1000}, "after_ms must be strictly less than before_ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.purgePreview(ctx, PurgePreviewInput{Filter: tc.filter}); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("preview error = %v, want one mentioning %q", err, tc.want)
			}
			if _, err := service.purge(ctx, PurgeInput{
				Filter: tc.filter, Confirmation: PurgeConfirmation, PreviewToken: "x",
			}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("purge error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
	if len(proxy.requests()) != 0 {
		t.Fatal("a filter the proxy would reject must not be sent")
	}
	// The band's own edges and a valid window are accepted.
	proxy.json(http.MethodPost, purgeCountPat, `{"count":1}`)
	for _, filter := range []PurgeFilterInput{
		{Provider: "p", StatusCode: 999},
		{Provider: "p", StatusCode: 1},
		{Provider: "p", BeforeMs: 1000, AfterMs: 999},
		{Provider: "p", AfterMs: 5},
		{Provider: "p", BeforeMs: 5},
	} {
		if err := filter.Validate(); err != nil {
			t.Fatalf("%+v is inside the proxy's rules: %v", filter, err)
		}
	}
}

// TestPurgeWarnsThatCapturesAreDeleted pins the truth about capture documents.
// A purge deletes the request_debug rows of every request it matches, in the
// same transaction; the tool and its output used to say the opposite, so an
// operator who believed their evidence survived a purge had lost it.
func TestPurgeWarnsThatCapturesAreDeleted(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodPost, purgeCountPat, `{"count":4}`)
	proxy.json(http.MethodPost, purgePath, `{"ok":true}`)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	preview, err := service.purgePreview(ctx, PurgePreviewInput{Filter: PurgeFilterInput{Provider: "local"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview.Captures, "stored capture document is deleted") {
		t.Fatalf("the preview must warn that captures go too: %q", preview.Captures)
	}
	if strings.Contains(preview.Captures, "never touches") || strings.Contains(purgeNote, "capture") {
		t.Fatalf("the capture claim must not survive anywhere: preview %q note %q", preview.Captures, purgeNote)
	}

	// A debug filter says so outright, because it definitely removes captures.
	debug, err := service.purgePreview(ctx, PurgePreviewInput{Filter: PurgeFilterInput{HasDebug: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(debug.Captures, "explicitly selects captured requests") {
		t.Fatalf("a debug filter must say it definitely removes captures: %q", debug.Captures)
	}

	out, err := service.purge(ctx, PurgeInput{
		Filter: PurgeFilterInput{Provider: "local"}, Confirmation: PurgeConfirmation,
		PreviewToken: preview.PreviewToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Captures, "stored capture document is deleted") {
		t.Fatalf("the deletion output must repeat the capture effect: %q", out.Captures)
	}
	// The residual TOCTOU gap is stated in both directions, not claimed away.
	for _, needle := range []string{"no shared transaction", "without having been previewed"} {
		if !strings.Contains(preview.Warning, needle) {
			t.Fatalf("the preview warning must state the residual gap (%q): %q", needle, preview.Warning)
		}
	}
	if !strings.Contains(purgePreviewWarning, "no shared transaction") {
		t.Fatal("the residual check/delete gap must be stated in the standing warning")
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

	filter := PurgeFilterInput{Client: "dev", HasError: true, StatusCode: 500, BeforeMs: 1000}
	preview, err := service.purgePreview(context.Background(), PurgePreviewInput{Filter: filter})
	if err != nil {
		t.Fatal(err)
	}
	out, err := service.purge(context.Background(), PurgeInput{
		Filter: filter, Confirmation: PurgeConfirmation, PreviewToken: preview.PreviewToken,
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
	// The pre-delete and post-delete counts both used the deleted filter.
	counts := proxy.requestsFor(http.MethodPost, purgeCountPat)
	if len(counts) != 3 {
		t.Fatalf("expected the preview, the pre-delete re-check and the recount, got %d", len(counts))
	}
	for i, count := range counts {
		if count.Body != requests[0].Body {
			t.Fatalf("count %d must use the deleted filter: %q vs %q", i, count.Body, requests[0].Body)
		}
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
	if out.Count != 17 || !out.Verified {
		t.Fatalf("preview output = %+v", out)
	}
	if out.PreviewToken == "" {
		t.Fatal("the preview must issue the token that authorizes the matching deletion")
	}
	// The residual gap is stated, not claimed away.
	if !strings.Contains(out.Warning, "no shared transaction") {
		t.Fatalf("preview warning = %q", out.Warning)
	}
	if !strings.Contains(out.Filter, "provider=local") || !strings.Contains(out.Filter, "before_ms=100") {
		t.Fatalf("the previewed filter must be stated: %q", out.Filter)
	}
	assertAuth(t, proxy.requestsFor(http.MethodPost, purgeCountPat)[0])
}

func throttleIntPtr(value int) *int { return &value }

func throttleInt64Ptr(value int64) *int64 { return &value }
