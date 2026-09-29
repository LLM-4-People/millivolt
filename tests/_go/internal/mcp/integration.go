package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// The fixture's port and database are PER RUN. A fixed port and a fixed scratch
// path made two concurrent runs collide - and the browser-fixtures CI step
// already occupies the port this one used - so the port is taken from the
// operating system and the database is unique, keeping this test off any
// instance an operator already has open. scripts/dev.sh keys its PID file and
// log on the port, so a unique port is a unique process identity too.
const integrationDBPrefix = "/tmp/millivolt/millivolt-dev-mcp-"
const (
	// fixtureClient is the client label the fixture traffic presents, so the
	// capture scope and the purge scope are both exact.
	fixtureClient = "mcp-integration-fixture"
)

// devScratchPaths returns the files one scripts/dev.sh instance creates for its
// port, plus this fixture's own scratch database. dev.sh names its binary
// "$DEV_BASE-bin", not "$DEV_BASE.bin": cleanup that spelled the wrong name left
// a stale multi-megabyte binary in /tmp/millivolt after every run.
//
// The names are not guessed: TestDevScratchPathsMatchDevShOwner checks them
// against scripts/dev.sh, so a rename in the lifecycle owner fails the suite
// instead of silently leaking files again.
func devScratchPaths(port string) []string {
	base := "/tmp/millivolt/millivolt-dev-" + port
	return []string{
		base + ".yaml", base + ".pid", base + ".log", base + "-bin",
		integrationDBPrefix + port + ".db",
		integrationDBPrefix + port + ".db-shm",
		integrationDBPrefix + port + ".db-wal",
	}
}

// TestDevScratchPathsMatchDevShOwner pins the integration cleanup to the names
// the lifecycle owner actually uses. The binary suffix was once written as
// ".bin" while dev.sh builds "-bin", so each live run leaked its binary.
func TestDevScratchPathsMatchDevShOwner(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "dev.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// dev.sh builds every port-keyed name from DEV_BASE; these literals are its
	// side of the contract.
	for _, owner := range []string{
		`CONFIG_FILE="$DEV_BASE.yaml"`,
		`PID_FILE="$DEV_BASE.pid"`,
		`LOG_FILE="$DEV_BASE.log"`,
		`BIN="$DEV_BASE-bin"`,
	} {
		if !bytes.Contains(script, []byte(owner)) {
			t.Fatalf("scripts/dev.sh no longer declares %s; devScratchPaths must follow the owner", owner)
		}
	}
	got := map[string]bool{}
	for _, path := range devScratchPaths("8081") {
		got[path] = true
	}
	for _, want := range []string{
		"/tmp/millivolt/millivolt-dev-8081.yaml",
		"/tmp/millivolt/millivolt-dev-8081.pid",
		"/tmp/millivolt/millivolt-dev-8081.log",
		"/tmp/millivolt/millivolt-dev-8081-bin",
	} {
		if !got[want] {
			t.Fatalf("devScratchPaths is missing %q, a file scripts/dev.sh creates", want)
		}
	}
}

// devInstance starts the private dev instance and returns the connected tool
// session. The instance is stopped through scripts/dev.sh when the test ends, on
// success and on failure alike.
func devInstance(t *testing.T) *liveTools {
	t.Helper()
	if os.Getenv(integrationEnv) == "" {
		t.Skipf("set %s=1 to run the live MCP integration test", integrationEnv)
	}
	root := repoRoot(t)
	origin, err := reserveLoopbackOrigin(t)
	if err != nil {
		t.Fatal(err)
	}
	port := portOf(t, origin)
	// The dev instance shares this process's environment, which is how the
	// operator credential reaches it.
	t.Setenv("MILLIVOLT_OPERATOR_TOKEN", integrationToken)
	t.Setenv("DEV_PORT", port)
	t.Setenv("DEV_DB", integrationDBPrefix+port+".db")
	t.Setenv("DEV_HOST", "127.0.0.1")

	// dev.sh keeps its port-keyed scratch set after a stop, and this run's
	// port is its own. Registered before the stop cleanup so it runs after it
	// (t.Cleanup is LIFO): leaving the scratch config behind lets a later,
	// unrelated fixture that exclusively creates the same port-keyed path
	// (cmd/stress) collide with it.
	t.Cleanup(func() {
		for _, path := range devScratchPaths(port) {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("remove dev scratch %s: %v", path, err)
			}
		}
	})

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

	service, err := NewService(origin, integrationToken, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return &liveTools{session: connect(t, service), service: service, origin: origin}
}

// reserveLoopbackOrigin takes a loopback port from the operating system and
// closes it again, which is the only race-free-enough way to get a port no other
// run is using. It is deliberately not a fixed number: the browser-fixtures CI
// step already binds one, and a collision there is two jobs silently sharing a
// proxy and a database.
func reserveLoopbackOrigin(t *testing.T) (string, error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return "", err
	}
	if port < 1024 {
		return "", fmt.Errorf("reserved port %d is below the range scripts/dev.sh accepts", port)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port), nil
}

func portOf(t *testing.T, origin string) string {
	t.Helper()
	parsed, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Port()
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
	// origin is this run's own proxy, never a fixed one.
	origin string
	// invoked records every tool name this run actually called. The test
	// compares it with the server's registered list at the end, so coverage of
	// the tool surface is proven by the calls made rather than by a
	// hand-maintained list of calls somebody must remember to update.
	invoked map[string]bool
}

// record notes one tool name before its call, so an expected tool error still
// counts as the tool having been exercised.
func (l *liveTools) record(name string) {
	if l.invoked == nil {
		l.invoked = map[string]bool{}
	}
	l.invoked[name] = true
}

// invoke calls one tool and returns its structured output.
func (l *liveTools) invoke(t *testing.T, name string, arguments map[string]any) map[string]any {
	t.Helper()
	l.record(name)
	return structured(t, call(t, l.session, name, arguments))
}

// invokeError calls one tool expecting a tool error, and returns its text.
func (l *liveTools) invokeError(t *testing.T, name string, arguments map[string]any) string {
	t.Helper()
	l.record(name)
	result := call(t, l.session, name, arguments)
	if !result.IsError {
		t.Fatalf("%s must fail here, got: %s", name, textOf(t, result))
	}
	return textOf(t, result)
}

// assertEveryToolWasInvoked compares the recorded invocations with the tool
// list the server actually registered, in both directions: a registered tool
// that no call reached and a call to a name that is not registered are both
// failures. The set equality is what turns "drives every registered tool"
// from a claim about this file into a checked property of the live session.
func (l *liveTools) assertEveryToolWasInvoked(t *testing.T) {
	t.Helper()
	listed, err := l.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, tool := range listed.Tools {
		registered[tool.Name] = true
	}
	var missing, unknown []string
	for name := range registered {
		if !l.invoked[name] {
			missing = append(missing, name)
		}
	}
	for name := range l.invoked {
		if !registered[name] {
			unknown = append(unknown, name)
		}
	}
	if len(missing) != 0 || len(unknown) != 0 {
		slices.Sort(missing)
		slices.Sort(unknown)
		t.Fatalf("the live test must drive every registered tool: registered but never invoked: %v; invoked but not registered: %v",
			missing, unknown)
	}
}

// valuesOf reads a values response into a set of the value names it carries.
func valuesOf(t *testing.T, document map[string]any, dim string) map[string]bool {
	t.Helper()
	if text(t, document, "dim") != dim {
		t.Fatalf("values dim = %v, want %q", document["dim"], dim)
	}
	found := map[string]bool{}
	for _, entry := range array(t, document, "values") {
		row, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("value entry = %T", entry)
		}
		found[text(t, row, "value")] = true
	}
	return found
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

// TestLiveDevInstanceExercisesEveryTool drives every registered tool against a
// real proxy: describe, query, values, explore, chart, records, snapshot,
// prometheus, the operator reads and mutations, config_get/set_config/
// reload_config, a full capture cycle (start, captured request, list, read
// back, stop), and both the purge guards and a successful purge. The mutations
// run only against this test's own disposable instance and scratch config.
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
	sendFixtureRequest(t, live.origin, upstream)
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

	// prometheus reads the exposition text.
	prom := live.invoke(t, "prometheus", map[string]any{})
	if number(t, prom, "lines") < 1 {
		t.Fatalf("the exposition must carry at least one line: %v", prom)
	}

	// The mutations below run against this test's own disposable instance.
	// A pause hold scoped to the fixture client, resumed at once.
	hold := live.invoke(t, "set_pause", map[string]any{
		"paused": true, "clients": []any{fixtureClient}, "duration": "15m",
	})
	if scope := text(t, hold, "scope"); scope != "clients "+fixtureClient {
		t.Fatalf("the hold scope must be the fixture client, got %q", scope)
	}
	unpaused := live.invoke(t, "set_pause", map[string]any{"paused": false})
	if unpaused["resumed"] != true {
		t.Fatalf("resuming every hold must report resumed: %v", unpaused)
	}

	// set_throttle on a known provider, then clear; resume_quota is a no-op
	// without an open gate and must still report the gate state. The provider
	// vocabulary is read from the operator state just fetched: the fixture
	// traffic has already taught the proxy the upstream label.
	provider := "integration-fixture-provider"
	if throttleState, ok := state["throttle"].(map[string]any); ok {
		if known, ok := throttleState["known_providers"].([]any); ok && len(known) > 0 {
			if name, ok := known[0].(string); ok && name != "" {
				provider = name
			}
		}
	}
	limit := live.invoke(t, "set_throttle", map[string]any{"provider": provider, "concurrency": 4})
	if _, ok := limit["state"].(map[string]any); !ok {
		t.Fatalf("set_throttle must report the resulting state: %v", limit)
	}
	if note := text(t, limit, "note"); !strings.Contains(note, "not strict fixed-window") {
		t.Fatalf("set_throttle must state its enforcement semantics: %q", note)
	}
	live.invoke(t, "set_throttle", map[string]any{"provider": provider, "clear": true})
	quota := live.invoke(t, "resume_quota", map[string]any{"provider": provider})
	if _, ok := quota["resumed"].(bool); !ok {
		t.Fatalf("resume_quota must report the gate state: %v", quota)
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
	sendFixtureRequest(t, live.origin, upstream)

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

	// config_get and values are the two discovery tools the corrected surface
	// adds, and both must work against a real proxy: config_get is what makes
	// set_config's own advice followable, and values is what stops a guessed
	// filter value from becoming a confident zero.
	configDocument := live.invoke(t, "config_get", map[string]any{})
	if text(t, configDocument, "revision") == "" {
		t.Fatalf("config_get must return the revision set_config has to echo: %v", configDocument)
	}
	// set_config writes this test's own scratch config copy, and reload_config
	// re-reads the same file. The patched value is the one already in force,
	// so the run does not change the fixture's behavior.
	effective, ok := configDocument["effective"].(map[string]any)
	if !ok {
		t.Fatalf("config_get must carry the effective values: %v", configDocument)
	}
	dashRows, ok := effective["dash_log_rows"].(float64)
	if !ok || dashRows < 1 {
		t.Fatalf("dash_log_rows must be an effective number: %v", effective["dash_log_rows"])
	}
	patched := live.invoke(t, "set_config", map[string]any{
		"values":   map[string]any{"dash_log_rows": int(dashRows)},
		"revision": text(t, configDocument, "revision"),
	})
	if patched["saved"] != true {
		t.Fatalf("set_config must persist the patch: %v", patched)
	}
	reloaded := live.invoke(t, "reload_config", map[string]any{})
	if reloaded["ok"] != true {
		t.Fatalf("reload_config must re-read the config file: %v", reloaded)
	}
	clientNames := valuesOf(t, live.invoke(t, "values", map[string]any{"dim": "client"}), "client")
	if !clientNames[fixtureClient] {
		t.Fatalf("values on client must discover the fixture client: %v", clientNames)
	}
	vocabularies, isList := reference["vocabularies"].([]any)
	if !isList || len(vocabularies) == 0 {
		t.Fatalf("describe must carry a vocabulary per dimension: %v", reference["vocabularies"])
	}
	dimensions := map[string]bool{}
	for _, entry := range vocabularies {
		vocabulary, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("vocabulary entry = %T", entry)
		}
		dimensions[text(t, vocabulary, "dimension")] = true
	}
	for _, dimension := range []string{"status", "time", "error", "client", "provider", "model", "tool"} {
		if !dimensions[dimension] {
			t.Fatalf("describe is missing the %q vocabulary: %v", dimension, dimensions)
		}
	}
	// A dimension the probe cannot enumerate is refused and named as such, and
	// the note on a real probe explains where its values DO come from - which is
	// what stops a model from writing time:24h and reading the empty result as
	// "no traffic at night".
	if message := live.invokeError(t, "values", map[string]any{"dim": "time"}); !strings.Contains(message, "not probeable") {
		t.Fatalf("a derived dimension must be refused by name: %q", message)
	}
	probed := live.invoke(t, "values", map[string]any{"dim": "client"})
	if note := text(t, probed, "note"); !strings.Contains(note, "time and error are not listed here") {
		t.Fatalf("the values note must explain the derived dimensions: %q", note)
	}

	preview := live.invoke(t, "purge_preview", map[string]any{
		"filter": map[string]any{"client": fixtureClient},
	})
	previewed := number(t, preview, "count")
	if previewed < 2 {
		t.Fatalf("the fixture scope must hold both requests, got %v", previewed)
	}
	previewToken := text(t, preview, "preview_token")
	if previewToken == "" {
		t.Fatalf("the preview must authorize the matching deletion: %v", preview)
	}
	// The preview states that captures go too, because they do.
	if captures := text(t, preview, "captures"); !strings.Contains(captures, "stored capture document is deleted") {
		t.Fatalf("the preview must warn about captures: %q", captures)
	}
	// A wrong phrase is refused by the handler and the absent argument is
	// rejected by the schema. The refusal text itself is pinned by the unit
	// suite; this only proves the live path returns an error result at all.
	live.invokeError(t, "purge", map[string]any{
		"filter":        map[string]any{"client": fixtureClient},
		"preview_token": previewToken,
	})
	live.invokeError(t, "purge", map[string]any{
		"filter":        map[string]any{"client": fixtureClient},
		"confirmation":  "delete everything",
		"preview_token": previewToken,
	})
	// A token for a DIFFERENT filter must not authorize this one, even though
	// both match the same rows. This is the bypass a bare count allowed.
	if message := live.invokeError(t, "purge", map[string]any{
		"filter":        map[string]any{"provider": "nonexistent-provider"},
		"confirmation":  PurgeConfirmation,
		"preview_token": previewToken,
	}); !strings.Contains(message, "DIFFERENT filter") {
		t.Fatalf("a token for another filter must refuse: %q", message)
	}
	if message := live.invokeError(t, "purge", map[string]any{
		"filter":        map[string]any{"client": fixtureClient},
		"confirmation":  PurgeConfirmation,
		"preview_token": "not-a-token",
	}); !strings.Contains(message, "preview_token") {
		t.Fatalf("an unauthorized token must refuse: %q", message)
	}
	after := live.invoke(t, "purge_preview", map[string]any{
		"filter": map[string]any{"client": fixtureClient},
	})
	if number(t, after, "count") != previewed {
		t.Fatalf("a refused purge changed history: %v then %v", previewed, after["count"])
	}
	// The rows are still readable, which is the observable proof nothing was
	// deleted by the refused calls above. The id the durability wait
	// established must be one of them: the old `recordID == ""` guard could
	// never fire, because eventually only returns after the probe set it, so
	// it asserted nothing. A mismatch here fails for real.
	readable := live.invoke(t, "query", map[string]any{
		"sql": "SELECT id FROM requests WHERE client = " + sqlString(fixtureClient) + " LIMIT 5",
	})
	found := false
	for _, entry := range array(t, readable, "rows") {
		row, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("query row = %T", entry)
		}
		if text(t, row, "id") == recordID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the durable record id %q is not among the live rows: %v", recordID, readable)
	}

	// The last call is the real deletion, on this disposable instance only:
	// the whole purge path runs, not just its guards.
	deleted := live.invoke(t, "purge", map[string]any{
		"filter":        map[string]any{"client": fixtureClient},
		"confirmation":  PurgeConfirmation,
		"preview_token": previewToken,
	})
	if deleted["deleted"] != true {
		t.Fatalf("the authorized purge must delete: %v", deleted)
	}
	if remaining := number(t, deleted, "remaining_count"); remaining != 0 {
		t.Fatalf("the fixture rows must be gone, got %v remaining", remaining)
	}

	// The claim in this file's own name is checked, not asserted by hand: the
	// set of tools called must equal the set the server registered.
	live.assertEveryToolWasInvoked(t)
}
