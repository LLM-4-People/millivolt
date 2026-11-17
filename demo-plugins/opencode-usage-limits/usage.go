// Package usagelimits is a reference v1 plugin that tracks observed request
// volume, token use, cost and rate-limit signals for one provider label. It
// uses the generic http:fetch capability to read OpenCode Go quota with the
// subscription API key; no product-specific endpoint lives in Millivolt core.
package usagelimits

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	v1 "github.com/LLM-4-People/millivolt-plugin-sdk/abi/v1"
)

const (
	PluginVersion         = "0.7.1"
	maxModels             = 64
	PanelID               = "opencode-usage-limits"
	ToolName              = "opencode_usage_summary"
	CommandReset          = "reset"
	emptyModel            = "unknown"
	overflowModel         = "__other__"
	defaultProvider       = "opencode-go"
	capabilityRecord      = "read:records"
	capabilityFetch       = v1.CapabilityHTTPFetch
	quotaURL              = "https://opencode.ai/zen/go/v1/usage"
	quotaCredentialID     = "request"
	quotaCredentialHeader = "Authorization"
)

type zenUsageWindow struct {
	Status   string  `json:"status"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resetsAt"`
}

type zenUsageResponse struct {
	Usage struct {
		Rolling zenUsageWindow `json:"rolling"`
		Weekly  zenUsageWindow `json:"weekly"`
		Monthly zenUsageWindow `json:"monthly"`
	} `json:"usage"`
}

type quotaMeter struct {
	Percent  float64
	Limited  bool
	ResetsAt int64
}

type quotaSnapshot struct {
	Rolling *quotaMeter
	Weekly  *quotaMeter
	Monthly *quotaMeter
	Status  string
}

// host is the capability-bearing boundary used by this guest. It is defined
// locally so the WASM adapter does not need to import the guest runner package.
type host interface {
	Log(level, message string) bool
	EmitMetric(name string, value float64, labels map[string]string) bool
	ReadRecord() (*v1.Record, bool)
	ReadConfigView() (*v1.ConfigView, bool)
	DeclareTool(name, description, capability string) bool
	DeclareUIPanel(id, title string, data map[string]string) bool
	HTTPFetch(req v1.HTTPFetchRequest) (*v1.HTTPFetchResponse, bool)
}

// Totals is one model's cumulative observed usage.
type Totals struct {
	Requests      int64
	InputTokens   int64
	OutputTokens  int64
	Cost          float64
	RateLimited   int
	LastRemaining int
}

// Plugin accumulates per-model totals from request.recorded events. A host is
// optional so the shared conformance suite can run without read capabilities;
// the WASM adapter injects one at init.
type Plugin struct {
	provider  string
	mu        sync.Mutex
	host      host
	totals    map[string]*Totals
	order     []string
	config    *v1.ConfigView
	reset     uint64
	lastModel string
	quota     quotaSnapshot
}

// New creates a plugin for one provider label. An empty label uses the
// opencode-go sidecar label; "*" accepts every provider (used by conformance).
func New(provider string) *Plugin {
	if provider == "" {
		provider = defaultProvider
	}
	if provider == "*" {
		provider = ""
	}
	return &Plugin{provider: provider, totals: map[string]*Totals{}, quota: quotaSnapshot{Status: "unavailable"}}
}

// SetHost injects the capability-bearing host before Init.
func (p *Plugin) SetHost(h host) {
	if p != nil {
		p.host = h
	}
}

func (p *Plugin) Init() error     { return nil }
func (p *Plugin) Shutdown() error { return nil }

// Handle implements guest.Guest.
func (p *Plugin) Handle(event v1.Event) (*v1.Response, error) {
	resp := &v1.Response{}
	switch event.Type {
	case v1.EventConfigChanged:
		if p.host == nil {
			return resp, nil
		}
		view := p.readConfig(event.Config)
		if view == nil {
			return resp, nil
		}
		p.mu.Lock()
		p.config = view
		p.mu.Unlock()
		resp.Logs = append(resp.Logs, v1.LogEntry{
			Level:   "info",
			Message: fmt.Sprintf("opencode config view updated: schema %d, %d values", view.SchemaVersion, len(view.Values)),
		})
		return resp, nil
	case v1.EventOperatorCommand:
		if event.Command == nil || event.Command.Name != CommandReset {
			return resp, nil
		}
		p.mu.Lock()
		p.totals = map[string]*Totals{}
		p.order = nil
		p.lastModel = ""
		p.reset++
		p.mu.Unlock()
		resp.Logs = append(resp.Logs, v1.LogEntry{Level: "info", Message: "opencode usage totals reset"})
		if p.host != nil {
			p.publish(resp)
		}
		return resp, nil
	case v1.EventRequestRecorded:
		rec := p.readRecord(event.Record)
		if rec == nil || (p.provider != "" && rec.Provider != p.provider) {
			return resp, nil
		}
		model := rec.Model
		if model == "" {
			model = emptyModel
		}
		rateLimited := rec.Status == 429 || strings.Contains(strings.ToLower(rec.ErrorType), "rate")
		total := p.add(rec.Provider, model, rec, rateLimited)
		labels := map[string]string{"provider": p.provider, "model": model}
		resp.Metrics = append(resp.Metrics,
			v1.Metric{Name: "plugin.opencode_limits.requests", Value: float64(total.Requests), Labels: labels},
			v1.Metric{Name: "plugin.opencode_limits.input_tokens", Value: float64(total.InputTokens), Labels: labels},
			v1.Metric{Name: "plugin.opencode_limits.output_tokens", Value: float64(total.OutputTokens), Labels: labels},
			v1.Metric{Name: "plugin.opencode_limits.cost", Value: total.Cost, Labels: labels},
			v1.Metric{Name: "plugin.opencode_limits.rate_limited", Value: float64(total.RateLimited), Labels: labels},
		)
		if total.LastRemaining > 0 {
			resp.Metrics = append(resp.Metrics, v1.Metric{
				Name: "plugin.opencode_limits.remaining", Value: float64(total.LastRemaining), Labels: labels,
			})
		}
		if rateLimited {
			resp.Logs = append(resp.Logs, v1.LogEntry{
				Level:   "warn",
				Message: fmt.Sprintf("%s rate-limited on %s", rec.Provider, model),
			})
		}
		p.refreshQuota()
		if p.host != nil {
			p.publish(resp)
		}
		return resp, nil
	default:
		return resp, nil
	}
}

func quotaMeterFromWindow(window zenUsageWindow) *quotaMeter {
	if window.Status != "" && !strings.EqualFold(window.Status, "ok") &&
		!strings.EqualFold(window.Status, "limited") {
		return nil
	}
	var resetsAt int64
	if window.ResetsAt != "" {
		if parsed, err := time.Parse(time.RFC3339, window.ResetsAt); err == nil {
			resetsAt = parsed.UnixMilli()
		}
	}
	return &quotaMeter{
		Percent:  window.Percent,
		Limited:  strings.EqualFold(window.Status, "limited") || window.Percent >= 100,
		ResetsAt: resetsAt,
	}
}

func (p *Plugin) refreshQuota() {
	if p.host == nil {
		return
	}
	resp, ok := p.host.HTTPFetch(v1.HTTPFetchRequest{
		URL:              quotaURL,
		Method:           "GET",
		CredentialID:     quotaCredentialID,
		CredentialHeader: quotaCredentialHeader,
		TimeoutMs:        1500,
		MaxResponseBytes: 64 << 10,
	})
	if !ok || resp == nil || resp.Error != "" || resp.Status < 200 || resp.Status >= 300 {
		p.setQuotaUnavailable()
		return
	}
	var payload zenUsageResponse
	if json.Unmarshal([]byte(resp.Body), &payload) != nil {
		p.setQuotaUnavailable()
		return
	}
	usage := payload.Usage
	if usage.Rolling.Percent == 0 && usage.Rolling.ResetsAt == "" &&
		usage.Weekly.Percent == 0 && usage.Weekly.ResetsAt == "" &&
		usage.Monthly.Percent == 0 && usage.Monthly.ResetsAt == "" {
		p.setQuotaUnavailable()
		return
	}
	p.mu.Lock()
	p.quota = quotaSnapshot{
		Rolling: quotaMeterFromWindow(usage.Rolling),
		Weekly:  quotaMeterFromWindow(usage.Weekly),
		Monthly: quotaMeterFromWindow(usage.Monthly),
		Status:  "ok",
	}
	p.mu.Unlock()
}

func (p *Plugin) setQuotaUnavailable() {
	p.mu.Lock()
	p.quota = quotaSnapshot{Status: "unavailable"}
	p.mu.Unlock()
}

func quotaFields(name string, meter *quotaMeter) string {
	if meter == nil {
		return name + ": -"
	}
	left := 100 - meter.Percent
	if left < 0 {
		left = 0
	}
	resets := "-"
	if meter.ResetsAt > 0 {
		resets = time.UnixMilli(meter.ResetsAt).UTC().Format(time.RFC3339)
	}
	return name + " used: " + fmt.Sprintf("%.0f%%", meter.Percent) +
		" | left: " + fmt.Sprintf("%.0f%%", left) +
		" | limited: " + fmt.Sprintf("%t", meter.Limited) +
		" | resets: " + resets
}

func (p *Plugin) readRecord(fallback *v1.Record) *v1.Record {
	if p != nil && p.host != nil {
		if record, ok := p.host.ReadRecord(); ok {
			return record
		}
	}
	return fallback
}

func (p *Plugin) readConfig(fallback *v1.ConfigView) *v1.ConfigView {
	if p != nil && p.host != nil {
		if view, ok := p.host.ReadConfigView(); ok {
			return view
		}
	}
	return fallback
}

func (p *Plugin) add(provider, model string, rec *v1.Record, rateLimited bool) Totals {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.totals[model]; !exists && len(p.totals) >= maxModels {
		model = overflowModel
	}
	total := p.totals[model]
	if total == nil {
		total = &Totals{}
		p.totals[model] = total
		p.order = append(p.order, model)
	}
	total.Requests++
	total.InputTokens += rec.InputTokens
	total.OutputTokens += rec.OutputTokens
	total.Cost += rec.Cost
	if rec.RateLimitRemaining > 0 {
		total.LastRemaining = rec.RateLimitRemaining
	}
	if rateLimited {
		total.RateLimited++
	}
	p.lastModel = model
	return *total
}

func (p *Plugin) publish(resp *v1.Response) {
	resp.Tools = append(resp.Tools, v1.ToolDescriptor{
		Name:        ToolName,
		Description: "Summarize observed OpenCode request usage and rate limits",
		Capability:  capabilityRecord,
	})
	resp.Panels = append(resp.Panels, v1.UIPanel{ID: PanelID, Title: "OpenCode Go usage", Data: p.PanelData()})
}

// PanelData is the bounded string view rendered by the dedicated dashboard tab.
func (p *Plugin) PanelData() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var requests, input, output, limited int64
	var cost float64
	remaining := 0
	for _, model := range p.order {
		total := p.totals[model]
		requests += total.Requests
		input += total.InputTokens
		output += total.OutputTokens
		cost += total.Cost
		limited += int64(total.RateLimited)
		if total.LastRemaining > remaining {
			remaining = total.LastRemaining
		}
	}
	status := "idle"
	if len(p.order) > 0 {
		status = "tracking"
	}
	schema, values := 0, 0
	if p.config != nil {
		schema, values = p.config.SchemaVersion, len(p.config.Values)
	}
	rollingQuota := quotaFields("rolling", p.quota.Rolling)
	weeklyQuota := quotaFields("weekly", p.quota.Weekly)
	monthlyQuota := quotaFields("monthly", p.quota.Monthly)
	quotaStatus := p.quota.Status
	if quotaStatus == "" {
		quotaStatus = "unavailable"
	}
	return map[string]string{
		"provider":       p.provider,
		"requests":       fmt.Sprintf("%d", requests),
		"models":         fmt.Sprintf("%d", len(p.order)),
		"input_tokens":   fmt.Sprintf("%d", input),
		"output_tokens":  fmt.Sprintf("%d", output),
		"cost":           fmt.Sprintf("%.6f", cost),
		"rate_limited":   fmt.Sprintf("%d", limited),
		"plugin_version": PluginVersion,
		"remaining":      fmt.Sprintf("%d", remaining),
		"last_model":     p.lastModel,
		"last_status":    status,
		"config_schema":  fmt.Sprintf("%d", schema),
		"config_values":  fmt.Sprintf("%d", values),
		"resets":         fmt.Sprintf("%d", p.reset),
		"model_list":     strings.Join(p.order, ", "),
		"quota_status":   quotaStatus,
		"rolling":        rollingQuota,
		"weekly":         weeklyQuota,
		"monthly":        monthlyQuota,
		"capability_set": "log, emit_metric, read:records, read:config, declare_tool, declare_ui_panel, http:fetch",
	}
}

// Totals returns a snapshot for tests and status reporting.
func (p *Plugin) Totals() map[string]Totals {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]Totals, len(p.totals))
	for model, total := range p.totals {
		out[model] = *total
	}
	return out
}
