package usagelimits

import (
	"strings"
	"testing"

	v1 "github.com/LLM-4-People/millivolt-plugin-sdk/abi/v1"
	"github.com/LLM-4-People/millivolt-plugin-sdk/conformance"
	"github.com/LLM-4-People/millivolt-plugin-sdk/guest/go"
	"github.com/LLM-4-People/millivolt-plugin-sdk/mock"
)

func recordEvent(provider, model string, status int, errorType string, remaining int, cost float64) v1.Event {
	return v1.Event{
		Type: v1.EventRequestRecorded,
		Record: &v1.Record{
			ID:                 "req-" + model,
			Provider:           provider,
			Model:              model,
			Status:             status,
			ErrorType:          errorType,
			RateLimitRemaining: remaining,
			Cost:               cost,
			InputTokens:        100,
			OutputTokens:       20,
		},
	}
}
func allCapabilitiesHost() *mock.Host {
	return mock.New(
		"log", "emit_metric", "read:records", "read:config", "declare_tool", "declare_ui_panel", v1.CapabilityHTTPFetch,
	)
}

func TestTracksOnlySelectedProvider(t *testing.T) {
	plugin := New("opencode-go")
	host := allCapabilitiesHost()
	plugin.SetHost(host)

	for _, event := range []v1.Event{
		recordEvent("opencode-go", "hy4-flash-free", 200, "", 480, 0.001),
		recordEvent("opencode-go", "hy4-flash-free", 429, "rate_limit", 120, 0.002),
		recordEvent("other.example", "foreign", 200, "", 99, 0.5),
	} {
		host.SetEvent(event)
		if _, err := guest.Run(plugin, host, event); err != nil {
			t.Fatal(err)
		}
	}

	totals := plugin.Totals()
	if len(totals) != 1 {
		t.Fatalf("Totals() = %+v, want one model", totals)
	}
	sum := totals["hy4-flash-free"]
	if sum.Requests != 2 || sum.RateLimited != 1 || sum.InputTokens != 200 || sum.LastRemaining != 120 {
		t.Fatalf("totals = %+v", sum)
	}
	if len(host.Metrics()) == 0 || len(host.Panels()) != 2 || len(host.Logs()) != 1 {
		t.Fatalf("metrics=%d panels=%d logs=%d", len(host.Metrics()), len(host.Panels()), len(host.Logs()))
	}
	if host.Panels()[1].ID != PanelID {
		t.Fatalf("panel = %+v", host.Panels()[1])
	}
	if len(host.Denials()) != 0 {
		t.Fatalf("unexpected denials: %v", host.Denials())
	}
}

func TestUsesAllSixCapabilities(t *testing.T) {
	plugin := New("opencode-go")
	host := allCapabilitiesHost()
	event := recordEvent("opencode-go", "hy4-flash-free", 200, "", 480, 0.001)
	config := &v1.ConfigView{SchemaVersion: 3, Values: map[string]string{"provider_aliases": "opencode-go"}}
	plugin.SetHost(host)
	host.SetEvent(event)

	if err := plugin.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := guest.Run(plugin, host, event); err != nil {
		t.Fatal(err)
	}
	configEvent := v1.Event{Type: v1.EventConfigChanged, Config: config}
	host.SetEvent(configEvent)
	if _, err := guest.Run(plugin, host, configEvent); err != nil {
		t.Fatal(err)
	}
	// A config event updates plugin state; the next tracked request refreshes
	// the panel that the dashboard reads.
	host.SetEvent(event)
	if _, err := guest.Run(plugin, host, event); err != nil {
		t.Fatal(err)
	}

	calls := strings.Join(host.Calls(), ",")
	for _, name := range []string{"read_record", "read_config_view", "log", "emit_metric", "declare_tool", "declare_ui_panel", "http_fetch"} {
		if !strings.Contains(calls, name) {
			t.Fatalf("host calls %q missing %s", calls, name)
		}
	}
	if denials := host.Denials(); len(denials) != 0 {
		t.Fatalf("unexpected denials: %v", denials)
	}
	if len(host.Tools()) == 0 || host.Tools()[0].Name != ToolName {
		t.Fatalf("tools = %+v", host.Tools())
	}
	panel := host.Panels()[len(host.Panels())-1]
	for _, key := range []string{"provider", "requests", "models", "input_tokens", "output_tokens", "cost", "rate_limited", "remaining", "config_schema", "config_values", "plugin_version"} {
		if _, ok := panel.Data[key]; !ok {
			t.Fatalf("panel data missing %q: %+v", key, panel.Data)
		}
	}
	if panel.Data["config_schema"] != "3" || panel.Data["config_values"] != "1" {
		t.Fatalf("config values not reported: %+v", panel.Data)
	}
	if panel.Data["plugin_version"] != PluginVersion {
		t.Fatalf("plugin version = %q, want %q", panel.Data["plugin_version"], PluginVersion)
	}
}

func TestOperatorResetClearsTotals(t *testing.T) {
	plugin := New("opencode-go")
	host := allCapabilitiesHost()
	event := recordEvent("opencode-go", "hy4-flash-free", 200, "", 480, 0.001)
	plugin.SetHost(host)
	host.SetEvent(event)
	if _, err := guest.Run(plugin, host, event); err != nil {
		t.Fatal(err)
	}
	if len(plugin.Totals()) != 1 {
		t.Fatal("record was not counted")
	}
	if _, err := guest.Run(plugin, host, v1.Event{Type: v1.EventOperatorCommand, Command: &v1.OperatorCommand{Name: CommandReset}}); err != nil {
		t.Fatal(err)
	}
	if totals := plugin.Totals(); len(totals) != 0 {
		t.Fatalf("totals after reset = %+v", totals)
	}
	if plugin.PanelData()["resets"] != "1" {
		t.Fatalf("panel data after reset = %+v", plugin.PanelData())
	}
}

func TestPassesSharedConformance(t *testing.T) {
	failures := conformance.Run(func() guest.Guest { return New("*") })
	for _, failure := range failures {
		t.Errorf("%s: %s", failure.Case, failure.Detail)
	}
}

func TestQuotaPanelFromHTTPFetch(t *testing.T) {
	plugin := New("opencode-go")
	host := allCapabilitiesHost()
	host.SetHTTPFetchResponse(&v1.HTTPFetchResponse{
		Status: 200,
		Body:   `{"usage":{"rolling":{"status":"ok","percent":62,"resetsAt":"2026-11-18T00:00:00Z"},"weekly":{"status":"ok","percent":81,"resetsAt":"2026-11-24T00:00:00Z"},"monthly":{"status":"ok","percent":37,"resetsAt":"2026-12-17T00:00:00Z"}}}`,
	})
	plugin.SetHost(host)
	event := recordEvent("opencode-go", "hy4-flash-free", 200, "", 480, 0.001)
	host.SetEvent(event)
	if _, err := guest.Run(plugin, host, event); err != nil {
		t.Fatal(err)
	}
	req := host.LastHTTPFetchRequest()
	if req.URL != quotaURL || req.CredentialID != quotaCredentialID || req.CredentialHeader != quotaCredentialHeader {
		t.Fatalf("fetch request = %+v", req)
	}
	if len(host.Panels()) != 1 {
		t.Fatalf("panels = %d, want 1", len(host.Panels()))
	}
	data := host.Panels()[0].Data
	if data["quota_status"] != "ok" || !strings.Contains(data["rolling"], "rolling used: 62%") ||
		!strings.Contains(data["rolling"], "left: 38%") {
		t.Fatalf("quota data = %+v", data)
	}
	if !strings.Contains(data["weekly"], "left: 19%") || !strings.Contains(data["monthly"], "left: 63%") {
		t.Fatalf("quota detail = %+v", data)
	}
}
