//go:build wasip1

// Command focus-wasm is the WASI guest adapter for the dashboard-focus SDK
// plugin. It uses only ABI v1 imports and exports mv_init/mv_handle/mv_shutdown.
package main

import (
	"encoding/json"

	v1 "github.com/LLM-4-People/millivolt-plugin-sdk/abi/v1"
	focus "github.com/LLM-4-People/millivolt-plugin-sdk"
	"github.com/LLM-4-People/millivolt-plugin-sdk/guest/go"
)

//go:wasmimport env mv_read_input
func mvReadInput(ptr *byte, capacity uint32) int32

//go:wasmimport env mv_write_output
func mvWriteOutput(ptr *byte, length uint32) int32

//go:wasmimport env mv_log
func mvLog(payload string) int32

//go:wasmimport env mv_emit_metric
func mvEmitMetric(payload string) int32

//go:wasmimport env mv_declare_ui_panel
func mvDeclareUIPanel(payload string) int32

//go:wasmimport env mv_declare_tool
func mvDeclareTool(payload string) int32

type wasmHost struct{}

func (wasmHost) Log(level, message string) bool {
	raw, err := json.Marshal(v1.LogEntry{Level: level, Message: message})
	if err != nil {
		return false
	}
	return mvLog(string(raw)) == 0
}

func (wasmHost) EmitMetric(name string, value float64, labels map[string]string) bool {
	raw, err := json.Marshal(v1.Metric{Name: name, Value: value, Labels: labels})
	if err != nil {
		return false
	}
	return mvEmitMetric(string(raw)) == 0
}

func (wasmHost) DeclareTool(name, description, capability string) bool {
	raw, err := json.Marshal(v1.ToolDescriptor{Name: name, Description: description, Capability: capability})
	if err != nil {
		return false
	}
	return mvDeclareTool(string(raw)) == 0
}

func (wasmHost) DeclareUIPanel(id, title string, data map[string]string) bool {
	raw, err := json.Marshal(v1.UIPanel{ID: id, Title: title, Data: data})
	if err != nil {
		return false
	}
	return mvDeclareUIPanel(string(raw)) == 0
}

func (wasmHost) HTTPFetch(v1.HTTPFetchRequest) (*v1.HTTPFetchResponse, bool) {
	return nil, false
}

func (wasmHost) ReadRecord() (*v1.Record, bool)         { return nil, false }
func (wasmHost) ReadConfigView() (*v1.ConfigView, bool) { return nil, false }

var (
	plugin *focus.Plugin
	input  = make([]byte, v1.MaxMessageBytes)
)

//go:wasmexport mv_init
func mvInit() int32 {
	plugin = focus.New()
	if plugin == nil {
		return 1
	}
	if err := plugin.Init(); err != nil {
		return 2
	}
	return 0
}

//go:wasmexport mv_handle
func mvHandle() int32 {
	if plugin == nil {
		return 3
	}
	n := mvReadInput(&input[0], uint32(len(input)))
	if n <= 0 {
		return 4
	}
	var event v1.Event
	if err := json.Unmarshal(input[:n], &event); err != nil {
		return 5
	}
	response, err := guest.Run(plugin, wasmHost{}, event)
	if err != nil {
		return 6
	}
	raw, err := json.Marshal(response)
	if err != nil || len(raw) == 0 {
		return 7
	}
	if mvWriteOutput(&raw[0], uint32(len(raw))) != 0 {
		return 8
	}
	return 0
}

//go:wasmexport mv_shutdown
func mvShutdown() int32 {
	if plugin == nil {
		return 0
	}
	if err := plugin.Shutdown(); err != nil {
		return 1
	}
	return 0
}

func main() {}
