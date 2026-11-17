// Package dashboardfocus is a visual UI sample plugin. It turns redacted
// request events into bounded, core-rendered descriptors that act like a small
// control deck: unicode meters, compact traffic rows and a composed OpenCode
// quota panel. It never supplies HTML or CSS and never modifies another guest.
package dashboardfocus

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	v1 "github.com/LLM-4-People/millivolt-plugin-sdk/abi/v1"
)

const (
	CommandReset      = "dashboard_focus_reset"
	PanelID           = "dashboard-focus"
	DefaultInclude    = "opencode-usage-limits"
	DefaultIncludeTab = "opencode-usage-limits"
	meterWidth        = 6
	maxRecent         = 10
	maxList           = 5
)

type totals struct {
	Requests      int64
	Input         int64
	Output        int64
	Errors        int64
	RateLimited   int64
	Cost          float64
	DurationMs    int64
	LastRemaining int
}

// Plugin accumulates a bounded traffic summary and publishes visual layout
// descriptors. Everything it emits is escaped text rendered by core widgets.
type Plugin struct {
	mu        sync.Mutex
	totals    map[string]*totals
	order     []string
	clients   map[string]int64
	recentTPS []float64
	lastModel string
	config    *v1.ConfigView
	resets    int
}

func New() *Plugin {
	return &Plugin{
		totals:  map[string]*totals{},
		clients: map[string]int64{},
	}
}

func (p *Plugin) Init() error     { return nil }
func (p *Plugin) Shutdown() error { return nil }

func (p *Plugin) Handle(event v1.Event) (*v1.Response, error) {
	resp := &v1.Response{}
	p.mu.Lock()
	switch event.Type {
	case v1.EventRequestRecorded:
		p.recordLocked(event.Record)
	case v1.EventConfigChanged:
		p.config = event.Config
	case v1.EventOperatorCommand:
		if event.Command != nil && event.Command.Name == CommandReset {
			p.totals = map[string]*totals{}
			p.order = nil
			p.clients = map[string]int64{}
			p.recentTPS = nil
			p.lastModel = ""
			p.resets++
			resp.Logs = append(resp.Logs, v1.LogEntry{Level: "info", Message: "dashboard focus totals reset"})
		}
	}
	data, panels := p.snapshotLocked()
	p.mu.Unlock()

	resp.Metrics = append(resp.Metrics, v1.Metric{
		Name:   "dashboard_focus_panels",
		Value:  float64(len(panels)),
		Labels: map[string]string{"view": "focus"},
	})
	resp.Panels = panels
	resp.Panels[0].Data = data
	return resp, nil
}

func (p *Plugin) recordLocked(rec *v1.Record) {
	if rec == nil {
		return
	}
	key := rec.Provider + "/" + rec.Model
	t := p.totals[key]
	if t == nil {
		t = &totals{}
		p.totals[key] = t
		p.order = append(p.order, key)
	}
	t.Requests++
	t.Input += rec.InputTokens
	t.Output += rec.OutputTokens
	t.Cost += rec.Cost
	t.DurationMs += rec.DurationMs
	if rec.Status >= 400 {
		t.Errors++
	}
	if rec.Status == 429 {
		t.RateLimited++
	}
	if rec.RateLimitRemaining > t.LastRemaining {
		t.LastRemaining = rec.RateLimitRemaining
	}
	if rec.Client != "" {
		p.clients[rec.Client]++
	}
	if rec.DurationMs > 0 && rec.OutputTokens > 0 {
		p.recentTPS = append(p.recentTPS, float64(rec.OutputTokens)*1000/float64(rec.DurationMs))
		if len(p.recentTPS) > maxRecent {
			p.recentTPS = p.recentTPS[len(p.recentTPS)-maxRecent:]
		}
	}
	p.lastModel = rec.Model
}

func (p *Plugin) snapshotLocked() (map[string]string, []v1.UIPanel) {
	var requests, input, output, errors, limited int64
	var cost float64
	for _, key := range p.order {
		t := p.totals[key]
		if t == nil {
			continue
		}
		requests += t.Requests
		input += t.Input
		output += t.Output
		errors += t.Errors
		limited += t.RateLimited
		cost += t.Cost
	}
	schema, values := 0, 0
	if p.config != nil {
		schema, values = p.config.SchemaVersion, len(p.config.Values)
	}
	health := meter(requests-errors, requests, meterWidth)
	traffic := meter(requests, max64(requests, 1), meterWidth)
	errorMeter := meter(errors, max64(requests, 1), meterWidth)
	topModel := topKey(p.totals, p.order)
	topClient := topCount(p.clients)
	tps := sparkline(p.recentTPS)
	modelList := compactList(p.order)
	clientList := compactCounts(p.clients)
	data := map[string]string{
		"view":             "focus",
		"include_plugin":   DefaultInclude,
		"include_panel":    DefaultIncludeTab,
		"arrange":          "▰▰▰▱▱ QUOTA · TRAFFIC · PLUGINS",
		"heading":          fmt.Sprintf("◉ Control deck · %d req · %d tok · $%.6f", requests, input+output, cost),
		"note":             fmt.Sprintf("▰ REQ %d %s | ▰ ERR %d %s | ▰ RL %d | ▰ TOK %d/%d | ▰ TPS %s | ▰ CLIENTS %s | QUOTA via opencode-usage-limits (unchanged)", requests, traffic, errors, errorMeter, limited, input, output, tps, clientList),
		"requests":         fmt.Sprintf("%d %s", requests, traffic),
		"models":           fmt.Sprintf("%d · %s", len(p.order), topModel),
		"input_tokens":     fmt.Sprintf("%d", input),
		"output_tokens":    fmt.Sprintf("%d", output),
		"errors":           fmt.Sprintf("%d %s", errors, health),
		"last_model":       p.lastModel,
		"config_schema":    fmt.Sprintf("%d", schema),
		"config_values":    fmt.Sprintf("%d", values),
		"model_list":       modelList,
		"layout_revisions": fmt.Sprintf("%d", p.resets),
		"cost":             fmt.Sprintf("%.6f", cost),
		"rate_limited":     fmt.Sprintf("%d", limited),
		"top_client":       topClient,
		"throughput_spark": tps,
	}
	rows := []struct {
		id    string
		title string
	}{
		{PanelID, "◉ FOCUS LAYOUT · quota + traffic + plugins"},
		{"visual-traffic", fmt.Sprintf("▰ TRAFFIC   %d req  %s", requests, traffic)},
		{"visual-health", fmt.Sprintf("▰ HEALTH    %d err  %s", errors, errorMeter)},
		{"visual-tokens", fmt.Sprintf("▰ TOKENS    %d in / %d out", input, output)},
		{"visual-models", fmt.Sprintf("▰ MODELS    %s", modelList)},
		{"visual-clients", fmt.Sprintf("▰ CLIENTS   %s", clientList)},
		{"visual-rate", fmt.Sprintf("▰ RATE      %d limited  %s", limited, sparkline(p.recentTPS))},
		{"visual-quota", "▰ QUOTA     composed from opencode-usage-limits · unchanged"},
	}
	panels := make([]v1.UIPanel, 0, len(rows))
	for _, row := range rows {
		panels = append(panels, v1.UIPanel{ID: row.id, Title: row.title, Data: map[string]string{
			"view": "visual-row", "row": row.title,
		}})
	}
	return data, panels
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func meter(value, max int64, width int) string {
	if max <= 0 || width <= 0 {
		return strings.Repeat("▱", width)
	}
	filled := int(value * int64(width) / max)
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return strings.Repeat("▰", filled) + strings.Repeat("▱", width-filled)
}

func sparkline(values []float64) string {
	if len(values) == 0 {
		return "▁▁▁▁▁▁"
	}
	steps := []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
	var out strings.Builder
	for _, value := range values {
		idx := int(value/8) % len(steps)
		if idx < 0 {
			idx = 0
		}
		out.WriteRune(steps[idx])
	}
	return out.String()
}

func topKey(counts map[string]*totals, order []string) string {
	best, bestN := "", int64(-1)
	for _, key := range order {
		if t := counts[key]; t != nil && t.Requests > bestN {
			best, bestN = key, t.Requests
		}
	}
	return best
}

func topCount(counts map[string]int64) string {
	best, bestN := "", int64(-1)
	for key, n := range counts {
		if n > bestN || (n == bestN && key < best) {
			best, bestN = key, n
		}
	}
	if best == "" {
		return "none"
	}
	return fmt.Sprintf("%s:%d", best, bestN)
}

func compactList(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	items := append([]string(nil), values...)
	if len(items) > maxList {
		items = append(items[:maxList:maxList], fmt.Sprintf("+%d", len(values)-maxList))
	}
	return strings.Join(items, ", ")
}

func compactCounts(counts map[string]int64) string {
	if len(counts) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] == counts[keys[j]] {
			return keys[i] < keys[j]
		}
		return counts[keys[i]] > counts[keys[j]]
	})
	items := make([]string, 0, maxList)
	for i, key := range keys {
		if i >= maxList {
			break
		}
		items = append(items, fmt.Sprintf("%s:%d", key, counts[key]))
	}
	return strings.Join(items, ", ")
}
