package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The live integration test owns a private dev instance through scripts/dev.sh,
// the repository's only dev-process lifecycle owner: `stop` is the only way
// down, and the instance is never the main one on :8080. It runs on its own
// loopback port with its own scratch database, so it cannot disturb an instance
// an operator already has open.
//
// It is opt-in, because starting a proxy and generating fixture traffic is not
// something a unit-suite run may do to a developer machine:
//
//	MILLIVOLT_MCP_INTEGRATION=1 scripts/check.sh go test -count=1 ./internal/mcp
//
// Every tool is invoked through a real MCP client session against the real
// registered tools, so broken wiring - a handler registered under the wrong
// name, or a request that no longer reaches its route - fails here.
const integrationEnv = "MILLIVOLT_MCP_INTEGRATION"

// integrationToken arms the dev instance's operator gate. It is a fixture value
// for a disposable scratch instance, not a credential for anything else.
const integrationToken = "millivolt-mcp-integration-credential"

// integrationPort and integrationDB keep the fixture off the default dev port
// and scratch database, so this test never restarts or resets an instance an
// operator already has open.
const (
	integrationPort   = "18081"
	integrationOrigin = "http://127.0.0.1:" + integrationPort
	integrationDB     = "/tmp/millivolt/millivolt-dev-mcp.db"
	// fixtureClient is the client label the fixture traffic presents, so the
	// capture scope and the purge scope are both exact.
	fixtureClient = "mcp-integration-fixture"
)

// devInstance starts the private dev instance and returns the connected tool
// session. The instance is stopped through scripts/dev.sh when the test ends, on
// success and on failure alike.
func devInstance(t *testing.T) *liveTools {
	t.Helper()
	if os.Getenv(integrationEnv) == "" {
		t.Skipf("set %s=1 to run the live MCP integration test", integrationEnv)
	}
	root := repoRoot(t)
	// The dev instance shares this process's environment, which is how the
	// operator credential reaches it.
	t.Setenv("MILLIVOLT_OPERATOR_TOKEN", integrationToken)
	t.Setenv("DEV_PORT", integrationPort)
	t.Setenv("DEV_DB", integrationDB)
	t.Setenv("DEV_HOST", "127.0.0.1")

	script := filepath.Join(root, "scripts", "dev.sh")
	start := exec.Command(script)
	start.Dir = root
	if output, err := start.CombinedOutput(); err != nil {
		t.Fatalf("scripts/dev.sh: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		stop := exec.Command(script, "stop")
		stop.Dir = root
		if output, err := stop.CombinedOutput(); err != nil {
			t.Errorf("scripts/dev.sh stop: %v\n%s", err, output)
		}
	})

	service, err := NewService(integrationOrigin, integrationToken, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return &liveTools{session: connect(t, service), service: service}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "scripts", "dev.sh")); err != nil {
		t.Fatalf("cannot locate the repository root from the test working directory: %v", err)
	}
	return root
}

// liveTools calls the registered tools through an MCP client session, so the
// wiring under test is the wiring a model would use.
type liveTools struct {
	session *sdk.ClientSession
	service *Service
}

// invoke calls one tool and returns its structured output.
func (l *liveTools) invoke(t *testing.T, name string, arguments map[string]any) map[string]any {
	t.Helper()
	return structured(t, call(t, l.session, name, arguments))
}

// invokeError calls one tool expecting a tool error, and returns its text.
func (l *liveTools) invokeError(t *testing.T, name string, arguments map[string]any) string {
	t.Helper()
	result := call(t, l.session, name, arguments)
	if !result.IsError {
		t.Fatalf("%s must fail here, got: %s", name, textOf(t, result))
	}
	return textOf(t, result)
}

// number reads one numeric field from a tool's structured output.
func number(t *testing.T, document map[string]any, key string) float64 {
	t.Helper()
	value, ok := document[key].(float64)
	if !ok {
		t.Fatalf("%s is %T, want a number (%v)", key, document[key], document)
	}
	return value
}

// text reads one string field from a tool's structured output.
func text(t *testing.T, document map[string]any, key string) string {
	t.Helper()
	value, ok := document[key].(string)
	if !ok {
		t.Fatalf("%s is %T, want a string (%v)", key, document[key], document)
	}
	return value
}

// array reads one array field from a tool's structured output.
func array(t *testing.T, document map[string]any, key string) []any {
	t.Helper()
	value, ok := document[key].([]any)
	if !ok {
		t.Fatalf("%s is %T, want an array (%v)", key, document[key], document)
	}
	return value
}

// fixtureUpstream is a neutral loopback upstream: never a paid provider, never
// the main instance. It answers one streamed completion so the proxy records a
// real request with usage and cost.
func fixtureUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(fixtureClient)) {
			// Only the integration fixture is answered. Anything else would be
			// a harness bug, not traffic to serve.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		const usage = `{"prompt_tokens":12,"completion_tokens":4,"total_tokens":16,"cost":0.0002}`
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":%s}\n\n", usage)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// sendFixtureRequest drives one request through the proxy to the neutral
// upstream, which is what makes a durable record and a capture exist.
func sendFixtureRequest(t *testing.T, origin string, upstream *httptest.Server) {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"model":    "mcp-integration-model",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": fixtureClient + " ping"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, origin+"/v1/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Proxy-Base-URL", upstream.URL)
	request.Header.Set("X-Proxy-Key", "local-fixture-only")
	request.Header.Set("X-Proxy-Client", fixtureClient)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("fixture request: %v", err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("fixture request status = %d, want 200", response.StatusCode)
	}
}

// eventually polls until the condition holds or the budget expires. Durable
// writes are asynchronous, so the only honest wait is a bounded poll on an
// observable condition; there is no fixed sleep anywhere in this file.
func eventually(t *testing.T, what string, probe func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if probe() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestLiveDevInstanceExercisesEveryTool drives the registered tools against a
// real proxy: describe, query, explore, chart, records, snapshot,
// operator_state, audit_status, a full capture cycle (start, captured request,
// list, read back, stop) and the purge-preview path.
func TestLiveDevInstanceExercisesEveryTool(t *testing.T) {
	live := devInstance(t)
	upstream := fixtureUpstream(t)

	// describe: the reference, with the live schema of a real database.
	reference := live.invoke(t, "describe", map[string]any{})
	objects := map[string]bool{}
	for _, entry := range array(t, reference, "schema") {
		object, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("schema entry = %T", entry)
		}
		objects[text(t, object, "name")] = true
	}
	for _, table := range []string{"requests", "request_debug", "meta", "request_projection_state"} {
		if !objects[table] {
			t.Fatalf("describe is missing the live %q table", table)
		}
	}
	storage, ok := reference["storage"].(map[string]any)
	if !ok || storage["enabled"] != true {
		t.Fatalf("the dev instance has a scratch database, so storage must be enabled: %v", reference["storage"])
	}

	// The first fixture request must become a durable row the tools can read.
	sendFixtureRequest(t, integrationOrigin, upstream)
	var recordID string
	eventually(t, "the fixture request to become durable", func() bool {
		rows := live.invoke(t, "query", map[string]any{
			"sql":      "SELECT id FROM requests WHERE client = " + sqlString(fixtureClient),
			"max_rows": 5,
		})
		found := array(t, rows, "rows")
		if len(found) == 0 {
			return false
		}
		row, ok := found[0].(map[string]any)
		if !ok {
			return false
		}
		recordID, _ = row["id"].(string)
		return recordID != ""
	})

	// An aggregate over the same predicate, proving the documented SQL works.
	aggregate := live.invoke(t, "query", map[string]any{
		"sql": "SELECT COUNT(*) AS n FROM requests WHERE client = " + sqlString(fixtureClient),
	})
	rows := array(t, aggregate, "rows")
	if len(rows) != 1 {
		t.Fatalf("aggregate query = %v", rows)
	}
	if count, ok := rows[0].(map[string]any)["n"].(float64); !ok || count < 1 {
		t.Fatalf("the fixture row must be counted: %v", rows[0])
	}

	// explore must carry the fixture client as one of its groups.
	explorer := live.invoke(t, "explore", map[string]any{"dim": "client"})
	names := []string{}
	for _, entry := range array(t, explorer, "groups") {
		group, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("explorer group = %T", entry)
		}
		name, _ := group["name"].(string)
		names = append(names, name)
	}
	if !slices.Contains(names, fixtureClient) {
		t.Fatalf("the fixture client is missing from the explorer: %v", names)
	}

	// chart must carry the bucket the fixture landed in.
	chart := live.invoke(t, "chart", map[string]any{"window": "60"})
	if len(array(t, chart, "buckets")) == 0 {
		t.Fatal("the chart must carry the fixture request's bucket")
	}
	if number(t, chart, "bucket_ms") <= 0 {
		t.Fatalf("bucket_ms = %v", chart["bucket_ms"])
	}

	// The durable log page answers with a real cursor.
	page := live.invoke(t, "records", map[string]any{"limit": 10})
	if number(t, page, "returned") < 1 {
		t.Fatalf("the durable log page returned nothing: %v", page)
	}
	if text(t, page, "next_before_id") == "" {
		t.Fatalf("the log page must hand back a cursor: %v", page)
	}

	snapshot := live.invoke(t, "snapshot", map[string]any{})
	snapshotStorage, ok := snapshot["storage"].(map[string]any)
	if !ok || snapshotStorage["enabled"] != true {
		t.Fatalf("snapshot storage = %v", snapshot["storage"])
	}

	state := live.invoke(t, "operator_state", map[string]any{})
	// problems is omitted when every surface answered.
	if problems, present := state["problems"]; present && len(problems.([]any)) != 0 {
		t.Fatalf("operator state surfaces failed: %v", problems)
	}
	for _, surface := range []string{"pause", "throttle", "quota", "restart"} {
		if _, ok := state[surface].(map[string]any); !ok {
			t.Fatalf("operator_state is missing %q: %v", surface, state)
		}
	}

	status := live.invoke(t, "audit_status", map[string]any{})
	statusStorage, ok := status["storage"].(map[string]any)
	if !ok || statusStorage["enabled"] != true {
		t.Fatalf("capture requires durable storage: %v", status["storage"])
	}
	// Sessions survive a proxy restart, so start from a known-clean state.
	live.invoke(t, "audit_stop", map[string]any{"stop_all": auditConfirmStopAll})

	started := live.invoke(t, "audit_start", map[string]any{
		"clients":  []any{fixtureClient},
		"duration": "15m",
		"confirm":  auditConfirmStart,
	})
	startedSession, ok := started["started"].(map[string]any)
	if !ok {
		t.Fatalf("audit_start output = %v", started)
	}
	sessionID := text(t, startedSession, "id")
	if sessionID == "" {
		t.Fatalf("audit_start returned no session id: %v", started)
	}

	// A second request while the session is live is what gets captured.
	sendFixtureRequest(t, integrationOrigin, upstream)

	var captureID string
	eventually(t, "a captured request to appear", func() bool {
		listed := live.invoke(t, "audit_captures_list", map[string]any{"session_id": sessionID})
		for _, entry := range array(t, listed, "records") {
			record, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if id, ok := record["id"].(string); ok {
				captureID = id
				return true
			}
		}
		return false
	})

	capture := live.invoke(t, "audit_capture_get", map[string]any{"record_id": captureID})
	document, ok := capture["document"].(map[string]any)
	if !ok || document == nil {
		t.Fatalf("the capture document must decode: %v", capture["document"])
	}
	if !strings.Contains(text(t, capture, "sensitive"), "BODIES") {
		t.Fatalf("the capture output must carry its sensitivity warning: %v", capture["sensitive"])
	}
	if number(t, capture, "stored_bytes") < 1 {
		t.Fatalf("the capture document is empty: %v", capture)
	}

	stopped := live.invoke(t, "audit_stop", map[string]any{"session_id": sessionID})
	if text(t, stopped, "stopped") != sessionID {
		t.Fatalf("audit_stop = %v", stopped)
	}
	// Stopping a session does not delete what it already captured.
	again := live.invoke(t, "audit_capture_get", map[string]any{"record_id": captureID})
	if number(t, again, "stored_bytes") != number(t, capture, "stored_bytes") {
		t.Fatalf("the capture changed across stop: %v then %v", capture["stored_bytes"], again["stored_bytes"])
	}

	preview := live.invoke(t, "purge_preview", map[string]any{
		"filter": map[string]any{"client": fixtureClient},
	})
	previewed := number(t, preview, "count")
	if previewed < 2 {
		t.Fatalf("the fixture scope must hold both requests, got %v", previewed)
	}
	// A purge without the exact confirmation cannot delete anything. The
	// required argument is enforced by the tool schema before the handler runs,
	// and a wrong phrase is refused by the handler: both must fail.
	if message := live.invokeError(t, "purge", map[string]any{
		"filter":         map[string]any{"client": fixtureClient},
		"reviewed_count": previewed,
	}); !strings.Contains(message, "confirmation") {
		t.Fatalf("the refusal must name the missing confirmation: %q", message)
	}
	if message := live.invokeError(t, "purge", map[string]any{
		"filter":         map[string]any{"client": fixtureClient},
		"confirmation":   "delete everything",
		"reviewed_count": previewed,
	}); !strings.Contains(message, PurgeConfirmation) {
		t.Fatalf("the refusal must name the required phrase: %q", message)
	}
	after := live.invoke(t, "purge_preview", map[string]any{
		"filter": map[string]any{"client": fixtureClient},
	})
	if number(t, after, "count") != previewed {
		t.Fatalf("a refused purge changed history: %v then %v", previewed, after["count"])
	}
	// The rows are still readable, which is the observable proof nothing was
	// deleted.
	live.invoke(t, "query", map[string]any{
		"sql": "SELECT id FROM requests WHERE client = " + sqlString(fixtureClient) + " LIMIT 5",
	})
	if recordID == "" {
		t.Fatal("the fixture record id was never established")
	}
}
