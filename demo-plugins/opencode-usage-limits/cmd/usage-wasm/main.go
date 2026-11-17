//go:build wasip1

// Command usage-wasm is a WASI guest adapter for the usage-limits SDK plugin.
// It uses the ABI v1 host imports and exports mv_init/mv_handle/mv_shutdown.
package main

import (
	"encoding/json"

	v1 "github.com/LLM-4-People/millivolt-plugin-sdk/abi/v1"
	"github.com/LLM-4-People/millivolt-plugin-sdk"
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

//go:wasmimport env mv_read_record
func mvReadRecord(payload string) int32

//go:wasmimport env mv_read_config_view
func mvReadConfigView(payload string) int32

//go:wasmimport env mv_declare_tool
func mvDeclareTool(payload string) int32

//go:wasmimport env mv_declare_ui_panel
func mvDeclareUIPanel(payload string) int32

//go:wasmimport env mv_http_fetch_request
func mvHTTPFetchRequest(payload string) int32

//go:wasmimport env mv_http_fetch_response
func mvHTTPFetchResponse(ptr *byte, capacity uint32) int32

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

func (wasmHost) ReadRecord() (*v1.Record, bool) {
	if mvReadRecord("{}") != 0 {
		return nil, false
	}
	return current.Record, current.Record != nil
}

func (wasmHost) ReadConfigView() (*v1.ConfigView, bool) {
	if mvReadConfigView("{}") != 0 {
		return nil, false
	}
	return current.Config, current.Config != nil
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

func (wasmHost) HTTPFetch(req v1.HTTPFetchRequest) (*v1.HTTPFetchResponse, bool) {
	raw, err := json.Marshal(req)
	if err != nil || len(raw) == 0 || len(raw) > v1.MaxMessageBytes {
		return nil, false
	}
	if mvHTTPFetchRequest(string(raw)) != 0 {
		return nil, false
	}
	n := mvHTTPFetchResponse(&fetchResponse[0], uint32(len(fetchResponse)))
	if n <= 0 || int(n) > len(fetchResponse) {
		return nil, false
	}
	var out v1.HTTPFetchResponse
	if json.Unmarshal(fetchResponse[:n], &out) != nil {
		return nil, false
	}
	return &out, true
}

var (
	plugin        *usagelimits.Plugin
	current       v1.Event
	input         = make([]byte, v1.MaxMessageBytes)
	fetchResponse = make([]byte, 80<<10)
)

//go:wasmexport mv_init
func mvInit() int32 {
	plugin = usagelimits.New("opencode-go")
	plugin.SetHost(wasmHost{})
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
	current = event
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
