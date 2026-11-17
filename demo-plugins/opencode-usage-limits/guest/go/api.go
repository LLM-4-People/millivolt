// Package guest is the Go-native guest API. A future core-WASM adapter will
// implement Host against the ABI imports without changing guest code.
package guest

import (
	"fmt"

	v1 "github.com/LLM-4-People/millivolt-plugin-sdk/abi/v1"
)

// Host is the capability-bearing boundary a guest may call. Every method
// returns false when the capability was not granted or the payload was too
// large; a guest must tolerate denial rather than failing the event.
type Host interface {
	Log(level, message string) bool
	EmitMetric(name string, value float64, labels map[string]string) bool
	ReadRecord() (*v1.Record, bool)
	ReadConfigView() (*v1.ConfigView, bool)
	DeclareTool(name, description, capability string) bool
	HTTPFetch(req v1.HTTPFetchRequest) (*v1.HTTPFetchResponse, bool)
	DeclareUIPanel(id, title string, data map[string]string) bool
}

// Guest is the lifecycle a plugin implements. Init and Shutdown are called
// once each; Handle receives one event at a time.
type Guest interface {
	Init() error
	Handle(v1.Event) (*v1.Response, error)
	Shutdown() error
}

// Run drives one event through a guest and applies its response to the host.
// It validates response caps before any side effect, so a malformed response
// cannot partially emit observations.
func Run(g Guest, h Host, event v1.Event) (*v1.Response, error) {
	if g == nil || h == nil {
		return nil, fmt.Errorf("guest and host are required")
	}
	resp, err := g.Handle(event)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("guest returned a nil response")
	}
	if len(resp.Logs) > v1.MaxLogs || len(resp.Metrics) > v1.MaxMetrics ||
		len(resp.Tools) > v1.MaxTools || len(resp.Panels) > v1.MaxPanels {
		return nil, fmt.Errorf("response exceeds a repeated-field cap")
	}
	for _, entry := range resp.Logs {
		if len(entry.Message) > v1.MaxLogBytes {
			return nil, fmt.Errorf("log message exceeds %d bytes", v1.MaxLogBytes)
		}
		h.Log(entry.Level, entry.Message)
	}
	for _, metric := range resp.Metrics {
		if len(metric.Name) > v1.MaxNameBytes {
			return nil, fmt.Errorf("metric name exceeds %d bytes", v1.MaxNameBytes)
		}
		h.EmitMetric(metric.Name, metric.Value, metric.Labels)
	}
	for _, tool := range resp.Tools {
		h.DeclareTool(tool.Name, tool.Description, tool.Capability)
	}
	for _, panel := range resp.Panels {
		h.DeclareUIPanel(panel.ID, panel.Title, panel.Data)
	}
	return resp, nil
}
