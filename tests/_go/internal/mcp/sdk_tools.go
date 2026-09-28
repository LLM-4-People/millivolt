package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect wires a real MCP server and client over an in-memory transport, the
// same path a stdio session uses. Everything below exercises the registered
// tools end to end: schema inference, argument validation, the call, and the
// structured result.
func connect(t *testing.T, service *Service) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	server := sdk.NewServer(&sdk.Implementation{Name: "millivolt", Version: "0.0.0"}, nil)
	service.Register(server)
	client := sdk.NewClient(&sdk.Implementation{Name: "mcp-test", Version: "0.0.0"}, nil)
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// call invokes one tool with JSON arguments and returns the result.
func call(t *testing.T, session *sdk.ClientSession, name string, arguments map[string]any) *sdk.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return result
}

// structured decodes a tool's structured content.
func structured(t *testing.T, result *sdk.CallToolResult) map[string]any {
	t.Helper()
	if result.IsError {
		t.Fatalf("tool returned an error result: %s", textOf(t, result))
	}
	if result.StructuredContent == nil {
		t.Fatalf("tool returned no structured content: %s", textOf(t, result))
	}
	// The decoded result carries structured content as a generic JSON value.
	switch content := result.StructuredContent.(type) {
	case map[string]any:
		return content
	case []byte:
		var document map[string]any
		if err := json.Unmarshal(content, &document); err != nil {
			t.Fatalf("structured content is not a JSON object: %v", err)
		}
		return document
	default:
		t.Fatalf("structured content has unexpected type %T", result.StructuredContent)
		return nil
	}
}

func textOf(t *testing.T, result *sdk.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*sdk.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	if b.Len() == 0 {
		t.Fatalf("result carried no text content: %+v", result.Content)
	}
	return b.String()
}

// TestRegisteredToolSurface pins the whole advertised tool list. A tool added
// or removed here is a deliberate change to the public surface.
func TestRegisteredToolSurface(t *testing.T) {
	session := connect(t, newTestService(t, newFakeProxy(t), Limits{}))
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*sdk.Tool{}
	for _, tool := range listed.Tools {
		got[tool.Name] = tool
	}
	want := []string{
		"describe", "query", "values", "explore", "chart", "records", "snapshot", "prometheus",
		"audit_status", "audit_start", "audit_stop", "audit_captures_list", "audit_capture_get",
		"operator_state", "set_pause", "set_throttle", "resume_quota", "set_config", "config_get",
		"reload_config", "purge_preview", "purge",
	}
	if len(got) != len(want) {
		t.Fatalf("tool count = %d, want %d (%v)", len(got), len(want), keys(got))
	}
	for _, name := range want {
		tool, ok := got[name]
		if !ok {
			t.Fatalf("tool %q is missing (%v)", name, keys(got))
		}
		if len(tool.Description) < 80 {
			t.Fatalf("tool %q needs an actionable description, got %q", name, tool.Description)
		}
		if tool.Annotations == nil {
			t.Fatalf("tool %q must declare its annotations", name)
		}
		// The only destructive tool is purge, and it is the only one whose
		// confirmation matters.
		destructive := tool.Annotations.DestructiveHint != nil && *tool.Annotations.DestructiveHint
		if destructive != (name == "purge") {
			t.Fatalf("tool %q destructive hint = %v", name, destructive)
		}
		if name == "purge" && !strings.Contains(tool.Description, "NO server-side confirmation") {
			t.Fatalf("the purge description must state that there is no server-side confirmation: %q", tool.Description)
		}
		if name == "audit_start" && !strings.Contains(tool.Description, retentionNote) {
			t.Fatalf("the audit description must state what stopping does not delete: %q", tool.Description)
		}
	}
	// Nothing that could reach the inference catch-all, and no restore path.
	for name := range got {
		switch name {
		case "describe", "query", "values", "explore", "chart", "records", "snapshot", "prometheus",
			"audit_status", "audit_start", "audit_stop", "audit_captures_list", "audit_capture_get",
			"operator_state", "set_pause", "set_throttle", "resume_quota", "set_config", "config_get",
			"reload_config", "purge_preview", "purge":
		default:
			t.Fatalf("unexpected tool %q is registered", name)
		}
	}
}

func keys(tools map[string]*sdk.Tool) []string {
	out := make([]string, 0, len(tools))
	for name := range tools {
		out = append(out, name)
	}
	return out
}

// TestRegisteredToolsReachTheProxy calls every read tool through the SDK and
// pins the request each one produced, so the registration cannot drift from the
// handlers it points at.
func TestRegisteredToolsReachTheProxy(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, schemaPath, `[{"type":"table","name":"requests","tbl_name":"requests","sql":"CREATE TABLE requests"}]`)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	proxy.json(http.MethodGet, debugPath, liveDebugStatus)
	proxy.json(http.MethodGet, explorerPath, `{"dim":"provider","total":1,"groups":[{"name":"local"}],"rail":{},"scope":{}}`)
	proxy.json(http.MethodGet, chartPath, `{"now_ms":1,"from_ms":0,"bucket_ms":1,"buckets":[]}`)
	proxy.json(http.MethodGet, logPath, `{"records":[{"id":"r1"}],"more":false,"cursor_ms":1,"cursor_id":"r1"}`)
	proxy.respond(http.MethodGet, prometheusPat, cannedResponse{Status: 200, ContentType: "text/plain", Body: "x 1\n"})
	proxy.json(http.MethodGet, pausePath, `{"ok":true,"paused":false}`)
	proxy.json(http.MethodGet, throttlePath, `{"ok":true,"throttles":[],"known_providers":[]}`)
	proxy.json(http.MethodGet, quotaPath, `{"ok":true}`)
	proxy.json(http.MethodGet, restartPath, `{"ok":true,"available":true}`)
	proxy.json(http.MethodGet, configPath, `{"revision":"rev","values":{},"effective":{},"defaults":{},`+
		`"fields":[],"restart_required":[],"writable":true,"path":"/etc/millivolt/proxy.yaml"}`)
	proxy.json(http.MethodPost, pausePath, `{"ok":true,"paused":true}`)
	proxy.json(http.MethodPost, throttlePath, `{"ok":true,"throttles":[],"known_providers":[]}`)
	proxy.json(http.MethodPost, quotaPath, `{"ok":true}`)
	proxy.json(http.MethodPost, configPath, `{"saved":true,"values":{},"restart_required":[]}`)
	proxy.json(http.MethodPost, reloadPath, `{"ok":true,"restart_required":[]}`)
	proxy.json(http.MethodPost, purgeCountPat, `{"count":2}`)
	proxy.json(http.MethodGet, schemaPath, `[{"value":"dev","requests":2}]`)
	proxy.json(http.MethodPost, debugPath, `{"ok":true,"enabled":true,"sessions":[{"id":"s1","ran_ms":1,"captures":0}],`+
		`"known_clients":[],"known_providers":[],"known_models":[],"ttl":"168h","max_bytes":"1MiB"}`)

	session := connect(t, newTestService(t, proxy, Limits{}))

	for _, tc := range []struct {
		tool      string
		arguments map[string]any
		wantKey   string
	}{
		{"describe", map[string]any{}, "schema"},
		{"query", map[string]any{"sql": "SELECT 1", "max_rows": 5}, "rows"},
		{"explore", map[string]any{"dim": "provider"}, "groups"},
		{"chart", map[string]any{"window": "5"}, "buckets"},
		{"records", map[string]any{"limit": 10}, "records"},
		{"snapshot", map[string]any{}, "records"},
		{"prometheus", map[string]any{}, "text"},
		{"config_get", map[string]any{}, "revision"},
		{"values", map[string]any{"dim": "client"}, "values"},
		{"audit_status", map[string]any{}, "sessions"},
		{"operator_state", map[string]any{}, "pause"},
		{"purge_preview", map[string]any{"filter": map[string]any{"provider": "local"}}, "count"},
	} {
		result := call(t, session, tc.tool, tc.arguments)
		document := structured(t, result)
		if _, present := document[tc.wantKey]; !present {
			t.Fatalf("%s output has no %q key: %v", tc.tool, tc.wantKey, keysOf(document))
		}
		text := textOf(t, result)
		if text == "" {
			t.Fatalf("%s must include human-legible content", tc.tool)
		}
		assertNoToken(t, tc.tool+" text", text)
	}

	// Mutations go through the same registration.
	mutations := []struct {
		tool      string
		arguments map[string]any
	}{
		{"audit_start", map[string]any{"clients": []any{"dev"}, "confirm": auditConfirmStart}},
		{"audit_stop", map[string]any{"session_id": "s1"}},
		{"set_pause", map[string]any{"paused": true, "all": true}},
		{"set_throttle", map[string]any{"provider": "local", "concurrency": 2}},
		{"resume_quota", map[string]any{"provider": "local"}},
		{"set_config", map[string]any{"values": map[string]any{"history_size": 100}, "revision": "rev"}},
		{"reload_config", map[string]any{}},
	}
	for _, tc := range mutations {
		result := call(t, session, tc.tool, tc.arguments)
		if result.IsError {
			t.Fatalf("%s returned an error result: %s", tc.tool, textOf(t, result))
		}
		assertNoToken(t, tc.tool+" text", textOf(t, result))
	}
	if len(proxy.requestsFor(http.MethodPost, purgePath)) != 0 {
		t.Fatal("no tool but purge may delete history")
	}
	for _, request := range proxy.requests() {
		assertAuth(t, request)
	}
}

// TestToolErrorsAreReadableResults pins that a proxy failure reaches the model
// as a tool result it can read, not as a transport error or a crash.
func TestToolErrorsAreReadableResults(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.fail(http.MethodGet, schemaPath, http.StatusUnauthorized, "operator token required", "")
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	proxy.json(http.MethodGet, debugPath, emptyDebugStatus)
	session := connect(t, newTestService(t, proxy, Limits{}))

	result := call(t, session, "query", map[string]any{"sql": "SELECT 1"})
	if !result.IsError {
		t.Fatal("a proxy failure must be an error result")
	}
	text := textOf(t, result)
	for _, needle := range []string{"401", "operator token required", "MILLIVOLT_OPERATOR_TOKEN"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("the tool error %q must mention %q", text, needle)
		}
	}
	assertNoToken(t, "tool error", text)
}

// TestInvalidArgumentsAreRejectedBeforeAnyRequest pins that the inferred input
// schema is enforced: a required argument missing never becomes a proxy call.
func TestInvalidArgumentsAreRejectedBeforeAnyRequest(t *testing.T) {
	proxy := newFakeProxy(t)
	session := connect(t, newTestService(t, proxy, Limits{}))
	for _, tc := range []struct {
		tool      string
		arguments map[string]any
	}{
		{"query", map[string]any{}},
		{"explore", map[string]any{}},
		{"audit_capture_get", map[string]any{}},
		{"purge", map[string]any{"filter": map[string]any{"provider": "p"}}},
	} {
		result := call(t, session, tc.tool, tc.arguments)
		if !result.IsError {
			t.Fatalf("%s must reject incomplete arguments", tc.tool)
		}
	}
	if len(proxy.requests()) != 0 {
		t.Fatal("a schema rejection must not reach the proxy")
	}
}

func keysOf(document map[string]any) []string {
	out := make([]string, 0, len(document))
	for key := range document {
		out = append(out, key)
	}
	return out
}

// TestSetPauseSchemaStatesTheGlobalBlastRadius pins the one place the tool told
// a model the opposite of the truth. The schema said "the proxy rejects a hold
// with no scope at all", which is the safe-sounding half of a global-hold
// default: the proxy does the opposite, an omitted scope IS a global hold, so
// leaving every scope field empty parks every client.
func TestSetPauseSchemaStatesTheGlobalBlastRadius(t *testing.T) {
	session := connect(t, newTestService(t, newFakeProxy(t), Limits{}))
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *sdk.Tool
	for _, candidate := range listed.Tools {
		if candidate.Name == "set_pause" {
			tool = candidate
		}
	}
	if tool == nil {
		t.Fatal("set_pause is not registered")
	}
	if !strings.Contains(tool.Description, "BLAST RADIUS") {
		t.Fatalf("the tool description must state the blast radius: %q", tool.Description)
	}
	if !strings.Contains(tool.Description, "GLOBAL hold") {
		t.Fatalf("the tool description must say an unscoped hold is global: %q", tool.Description)
	}
	// The inferred schema is what the model reads when composing arguments, so
	// the false claim must be gone from there too.
	schema, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	text := string(schema)
	if strings.Contains(text, "rejects a hold with no scope at all") {
		t.Fatalf("the schema must not claim a scopeless hold is rejected: %s", text)
	}
	if !strings.Contains(text, "the SAME global hold") {
		t.Fatalf("the `all` schema must say an omitted scope is the same global hold: %s", text)
	}
}
