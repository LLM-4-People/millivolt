package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const storageOffBootstrap = `{"seq":1,"feed_id":"f","counters":{},"storage":{"enabled":false},"records":[]}`

// TestAuditStatusReportsStorageAndVocabularies pins the two facts a caller must
// have before starting a capture: durable storage is on, and the valid scope
// names.
func TestAuditStatusReportsStorageAndVocabularies(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	proxy.json(http.MethodGet, debugPath, `{"ok":true,"enabled":true,"sessions":[{"id":"s1","clients":["dev"],"providers":null,`+
		`"models":["demo"],"duration":"15m","until":"2026-01-01T00:00:00Z","ran_ms":900,"captures":7}],`+
		`"known_clients":["dev","cli"],"known_providers":["local"],"known_models":["demo"],"ttl":"168h","max_bytes":"1MiB",`+
		`"warning":"State applied in memory, but could not be saved for restart."}`)

	service := newTestService(t, proxy, Limits{})
	out, err := service.auditStatus(context.Background(), AuditStatusInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Enabled || len(out.Sessions) != 1 {
		t.Fatalf("sessions = %+v", out.Sessions)
	}
	session := out.Sessions[0]
	if session.ID != "s1" || session.Captures != 7 || session.Duration != "15m" || session.RanMs != 900 {
		t.Fatalf("session mapping = %+v", session)
	}
	// A JSON null scope stays nil, which is how an empty dimension (matching
	// anything) is represented.
	if session.Clients[0] != "dev" || session.Providers != nil || session.Models[0] != "demo" {
		t.Fatalf("session scope mapping = %+v", session)
	}
	if !out.Storage.Enabled || out.TTL == "" || out.MaxBytes == "" {
		t.Fatalf("capture budget mapping = %+v", out)
	}
	if !strings.Contains(out.Retention, "does NOT delete") {
		t.Fatalf("retention note = %q", out.Retention)
	}
	if !strings.Contains(out.Warning, "could not be saved") {
		t.Fatalf("persistence warning must survive: %q", out.Warning)
	}
	// The read is two GETs, both authenticated, and it never writes.
	for _, path := range []string{debugPath, bootstrapPath} {
		calls := proxy.requestsFor(http.MethodGet, path)
		if len(calls) != 1 {
			t.Fatalf("expected exactly one GET %s, got %d", path, len(calls))
		}
		assertAuth(t, calls[0])
	}
	if len(proxy.requestsFor(http.MethodPost, debugPath)) != 0 {
		t.Fatal("audit_status must never mutate the session state")
	}
}

// TestAuditStartRefusesWithoutDurableStorage is the prerequisite: a session
// would start, report success, and capture nothing.
func TestAuditStartRefusesWithoutDurableStorage(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, storageOffBootstrap)
	service := newTestService(t, proxy, Limits{})
	_, err := service.auditStart(context.Background(), AuditStartInput{
		Clients: []string{"dev"}, Duration: "15m", Confirm: auditConfirmStart,
	})
	if err == nil {
		t.Fatal("starting a capture without durable storage must be refused")
	}
	for _, needle := range []string{"durable storage is disabled", "db_path", "nothing could ever be captured"} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("error %q must mention %q", err, needle)
		}
	}
	if len(proxy.requestsFor(http.MethodPost, debugPath)) != 0 {
		t.Fatal("no session may be started when nothing could be captured")
	}
}

// TestAuditStartGuards pins the confirmation, the scope requirement and the
// duration allowlist, none of which may reach the proxy when violated.
func TestAuditStartGuards(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		in   AuditStartInput
		want string
	}{
		{"no confirmation", AuditStartInput{Clients: []string{"dev"}}, "confirm must be"},
		{"wrong confirmation", AuditStartInput{Clients: []string{"dev"}, Confirm: "yes"}, "confirm must be"},
		{"no scope", AuditStartInput{Confirm: auditConfirmStart}, "at least one of clients, providers or models"},
		{"bad duration", AuditStartInput{Clients: []string{"dev"}, Confirm: auditConfirmStart, Duration: "30m"}, "15m, 1h, 6h, 12h, 24h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.auditStart(ctx, tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
	if len(proxy.requestsFor(http.MethodPost, debugPath)) != 0 {
		t.Fatal("a refused start must not reach the proxy")
	}
}

// TestAuditStartBodyShape pins that only a named dimension is sent: an omitted
// scope is preserved on an edit and rejected on a create, so the tool must not
// substitute an empty array for it.
func TestAuditStartBodyShape(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	proxy.json(http.MethodPost, debugPath, `{"ok":true,"enabled":true,"sessions":[{"id":"s1","clients":["dev"],"ran_ms":1,"captures":0}],`+
		`"known_clients":["dev"],"known_providers":[],"known_models":[],"ttl":"168h","max_bytes":"1MiB"}`)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	if _, err := service.auditStart(ctx, AuditStartInput{Clients: []string{"dev"}, Duration: "15m", Confirm: auditConfirmStart}); err != nil {
		t.Fatal(err)
	}
	body := decodeBody(t, proxy.requestsFor(http.MethodPost, debugPath)[0])
	assertAuth(t, proxy.requestsFor(http.MethodPost, debugPath)[0])
	if body["enabled"] != true {
		t.Fatalf("enabled = %v, want true", body["enabled"])
	}
	for _, absent := range []string{"id", "providers", "models"} {
		if _, present := body[absent]; present {
			t.Fatalf("an unnamed dimension must be omitted, but %q was sent: %v", absent, body)
		}
	}
	if body["duration"] != "15m" {
		t.Fatalf("duration = %v", body["duration"])
	}
	if _, present := body["confirm"]; present {
		t.Fatal("the confirmation is a tool-level guard and must never reach the proxy")
	}

	if _, err := service.auditStart(ctx, AuditStartInput{
		SessionID: "s1", Confirm: auditConfirmStart,
	}); err != nil {
		t.Fatal(err)
	}
	edit := decodeBody(t, proxy.requestsFor(http.MethodPost, debugPath)[1])
	if edit["id"] != "s1" {
		t.Fatalf("edit must target the session: %v", edit)
	}
	if _, present := edit["duration"]; present {
		t.Fatalf("an omitted duration must be preserved by omission, not sent as empty: %v", edit)
	}
}

// TestAuditStartSurfacesOverlapConflict pins that a 409 reaches the model as a
// readable conflict rather than a silent success.
func TestAuditStartSurfacesOverlapConflict(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	proxy.fail(http.MethodPost, debugPath, http.StatusConflict, "debug session overlaps existing session", "")
	service := newTestService(t, proxy, Limits{})
	_, err := service.auditStart(context.Background(), AuditStartInput{
		Providers: []string{"local"}, Confirm: auditConfirmStart,
	})
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("error = %v, want the proxy's 409 message", err)
	}
}

// TestAuditStopGuards pins that stopping one session requires its id, that
// stopping everything requires the exact token, and that an id and stop-all
// together are ambiguous and refused.
func TestAuditStopGuards(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodPost, debugPath, `{"ok":true,"enabled":false,"sessions":[],"known_clients":[],`+
		`"known_providers":[],"known_models":[],"ttl":"168h","max_bytes":"1MiB"}`)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		in   AuditStopInput
		want string
	}{
		{"nothing", AuditStopInput{}, "session_id is required"},
		{"wrong stop-all token", AuditStopInput{StopAll: "yes"}, "stop_all must be"},
		{"both", AuditStopInput{SessionID: "s1", StopAll: auditConfirmStopAll}, "not both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.auditStop(ctx, tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
	if len(proxy.requestsFor(http.MethodPost, debugPath)) != 0 {
		t.Fatal("a refused stop must not reach the proxy")
	}

	out, err := service.auditStop(ctx, AuditStopInput{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stopped != "s1" {
		t.Fatalf("stopped = %q", out.Stopped)
	}
	body := decodeBody(t, proxy.requestsFor(http.MethodPost, debugPath)[0])
	if body["enabled"] != false || body["id"] != "s1" {
		t.Fatalf("stop body = %v", body)
	}

	if _, err := service.auditStop(ctx, AuditStopInput{StopAll: auditConfirmStopAll}); err != nil {
		t.Fatal(err)
	}
	all := decodeBody(t, proxy.requestsFor(http.MethodPost, debugPath)[1])
	if all["enabled"] != false {
		t.Fatalf("stop-all body = %v", all)
	}
	if _, present := all["id"]; present {
		t.Fatalf("stop-all must not carry an id: %v", all)
	}
	if !strings.Contains(out.Retention, "no delete") {
		t.Fatalf("the retention note must be in the output: %q", out.Retention)
	}
}

// TestAuditCapturesListPagesThroughSQL pins the mechanism, the page and the
// exhaustion signal. There is no listing endpoint, so the tool is a bounded
// SELECT over requests WHERE debug = 1.
func TestAuditCapturesListPagesThroughSQL(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	proxy.json(http.MethodGet, schemaPath,
		`[{"id":"c2","started_at":1700,"status_code":200,"provider":"local","model":"demo","client":"dev","debug_session_id":"s1","cost":0.1},`+
			`{"id":"c1","started_at":1600,"status_code":500,"provider":"local","model":"demo","client":"dev","debug_session_id":"s1","cost":0}]`)
	limits := DefaultLimits()
	limits.PageSize = 2
	service := newTestService(t, proxy, limits)

	out, err := service.auditCapturesList(context.Background(), AuditCapturesListInput{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Returned != 2 || !out.More {
		t.Fatalf("a full page means more may exist: %+v", out)
	}
	if out.NextBeforeID != "c1" || out.NextBeforeMs != 1600 {
		t.Fatalf("cursor = %+v", out)
	}
	if !strings.Contains(out.Note, "no capture listing endpoint") {
		t.Fatalf("the mechanism must be stated: %q", out.Note)
	}

	probe := proxy.requestsFor(http.MethodGet, schemaPath)[0]
	assertAuth(t, probe)
	statement := probe.Query.Get("q")
	for _, needle := range []string{"FROM requests", "WHERE debug = 1", "debug_session_id = 's1'", "ORDER BY started_at DESC, id DESC", "LIMIT 2"} {
		if !strings.Contains(statement, needle) {
			t.Fatalf("capture listing statement %q must contain %q", statement, needle)
		}
	}

	// A session id with a quote is a literal, never an expression.
	if _, err := service.auditCapturesList(context.Background(), AuditCapturesListInput{SessionID: "s'1"}); err != nil {
		t.Fatal(err)
	}
	quoted := proxy.requestsFor(http.MethodGet, schemaPath)[1].Query.Get("q")
	if !strings.Contains(quoted, "debug_session_id = 's''1'") {
		t.Fatalf("a quoted session id must be escaped as a literal: %q", quoted)
	}

	// The cursor pages keyset-style.
	if _, err := service.auditCapturesList(context.Background(), AuditCapturesListInput{
		BeforeMs: 1600, BeforeID: "c1", Limit: 2,
	}); err != nil {
		t.Fatal(err)
	}
	paged := proxy.requestsFor(http.MethodGet, schemaPath)[2].Query.Get("q")
	if !strings.Contains(paged, "(started_at, id) < (1600, 'c1')") {
		t.Fatalf("paged statement = %q", paged)
	}
}

// TestAuditCapturesListExhaustion pins the non-advancing-cursor rule for the
// capture listing: a page that cannot advance, or that is short, ends the walk.
func TestAuditCapturesListExhaustion(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	// A short page is the end of the list, not an invitation to keep paging.
	proxy.json(http.MethodGet, schemaPath, `[{"id":"c1","started_at":10}]`)
	short, err := service.auditCapturesList(ctx, AuditCapturesListInput{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if short.More || !short.Exhausted {
		t.Fatalf("a short page must be exhaustion: %+v", short)
	}

	// A full page whose oldest row repeats the cursor that was sent cannot
	// advance: the next call would return the same rows forever.
	proxy.json(http.MethodGet, schemaPath, `[{"id":"c2","started_at":10},{"id":"c1","started_at":10}]`)
	stuck, err := service.auditCapturesList(ctx, AuditCapturesListInput{BeforeMs: 10, BeforeID: "c1", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !stuck.Exhausted {
		t.Fatalf("a non-advancing cursor must be exhaustion: %+v", stuck)
	}

	// No captures at all is exhaustion too.
	proxy.json(http.MethodGet, schemaPath, `[]`)
	none, err := service.auditCapturesList(ctx, AuditCapturesListInput{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !none.Exhausted || none.Returned != 0 {
		t.Fatalf("an empty list must be exhaustion: %+v", none)
	}
}

// TestAuditCapturesListRefusesWithoutStorage pins the same prerequisite as the
// start path.
func TestAuditCapturesListRefusesWithoutStorage(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, storageOffBootstrap)
	service := newTestService(t, proxy, Limits{})
	if _, err := service.auditCapturesList(context.Background(), AuditCapturesListInput{}); err == nil ||
		!strings.Contains(err.Error(), "durable storage is disabled") {
		t.Fatalf("error = %v, want the storage prerequisite", err)
	}
	if len(proxy.requestsFor(http.MethodGet, schemaPath)) != 0 {
		t.Fatal("no listing may run without durable storage")
	}
}

// TestAuditCaptureGetPinsRecordIDAndSensitivity pins that the id is the request
// record id, that the document is returned as stored, and that the sensitivity
// warning travels with it.
func TestAuditCaptureGetPinsRecordIDAndSensitivity(t *testing.T) {
	document := `{"captured_at":"2026-01-01T00:00:00Z","request":{"body":{"messages":[{"content":"secret prompt"}]}},` +
		`"response":{"status_code":200,"body":{"choices":[]}}}`
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, capturePath, document)
	service := newTestService(t, proxy, Limits{})

	out, err := service.auditCaptureGet(context.Background(), AuditCaptureGetInput{RecordID: "rec-1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.RecordID != "rec-1" || out.StoredBytes != len(document) {
		t.Fatalf("capture output = %+v", out)
	}
	decoded, ok := out.Document.(map[string]any)
	if !ok {
		t.Fatalf("the document must be returned as stored JSON: %T", out.Document)
	}
	if decoded["captured_at"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("document mapping = %+v", decoded)
	}
	if !strings.Contains(out.Sensitive, "BODIES") {
		t.Fatalf("sensitivity warning = %q", out.Sensitive)
	}

	request := proxy.requestsFor(http.MethodGet, capturePath)[0]
	assertAuth(t, request)
	assertQuery(t, request, "id", "rec-1")
	assertNoQuery(t, request, "download")

	// An oversized document is withheld whole, never truncated into broken
	// JSON: the model raises max_bytes instead of reading a fragment.
	truncated, err := service.auditCaptureGet(context.Background(), AuditCaptureGetInput{RecordID: "rec-1", MaxBytes: 40})
	if err != nil {
		t.Fatal(err)
	}
	if !truncated.Truncation.Truncated || truncated.Document != nil || truncated.StoredBytes != len(document) {
		t.Fatalf("bounded capture = %+v", truncated)
	}
	if !strings.Contains(truncated.Truncation.Marker, "max_bytes") {
		t.Fatalf("marker must name the way to continue: %q", truncated.Truncation.Marker)
	}
}

// TestAuditCaptureGetErrors pins the missing-id and expired-document paths.
func TestAuditCaptureGetErrors(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.fail(http.MethodGet, capturePath, http.StatusNotFound, "debug capture not found", "")
	service := newTestService(t, proxy, Limits{})
	if _, err := service.auditCaptureGet(context.Background(), AuditCaptureGetInput{RecordID: "  "}); err == nil ||
		!strings.Contains(err.Error(), "record_id is required") {
		t.Fatalf("error = %v, want the missing-id message", err)
	}
	_, err := service.auditCaptureGet(context.Background(), AuditCaptureGetInput{RecordID: "gone"})
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "debug capture not found") {
		t.Fatalf("error = %v, want the proxy's 404", err)
	}
}
