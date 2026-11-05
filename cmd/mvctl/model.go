package main

// Wire views of the operator documents mvctl renders. Commands that own a safe
// wire view keep its decoded document for --json; status and overview use an
// explicit projection because the bootstrap payload also carries request rows
// and the effective config.

import (
	"encoding/json"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/mcp"
)

type KPI struct {
	Requests        float64  `json:"requests"`
	Errors          float64  `json:"errors"`
	InFlight        float64  `json:"in_flight"`
	Cost            float64  `json:"cost"`
	CostPerReq      *float64 `json:"cost_per_req"`
	CostPerMTok     *float64 `json:"cost_per_mtok"`
	InputTokens     float64  `json:"input_tokens"`
	OutputTokens    float64  `json:"output_tokens"`
	CacheReadTokens float64  `json:"cache_read_tokens"`
	ReasoningTokens float64  `json:"reasoning_tokens"`
	AvgTTFTMs       *float64 `json:"avg_ttft_ms"`
	AvgTPS          *float64 `json:"avg_tps"`
}

type StorageState struct {
	Enabled        bool    `json:"enabled"`
	Dropped        float64 `json:"dropped"`
	TotalsDegraded bool    `json:"totals_degraded"`
}

type PauseState struct {
	Paused bool    `json:"paused"`
	Queued float64 `json:"queued"`
}

type ThrottleState struct {
	Active    bool             `json:"active"`
	Throttles []map[string]any `json:"throttles"`
}

type DebugState struct {
	Enabled  bool             `json:"enabled"`
	Sessions []map[string]any `json:"sessions"`
}

type Snapshot struct {
	KPI              KPI           `json:"kpi"`
	Storage          StorageState  `json:"storage"`
	Pause            PauseState    `json:"pause"`
	Throttle         ThrottleState `json:"throttle"`
	Debug            DebugState    `json:"debug"`
	DashboardVersion string        `json:"dashboard_version"`
}

func decodeSnapshot(raw map[string]any) (Snapshot, error) {
	var snapshot Snapshot
	err := remarshal(raw, &snapshot)
	return snapshot, err
}

type Usage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// RequestRecord is the safe subset of one request-log row. Content previews,
// response headers and retry attempts stay out of the command's output: the
// dashboard deliberately fetches a capture drawer only on demand, and a
// terminal pipeline is not a place to spill captured bodies by default.
type RequestRecord struct {
	ID           string    `json:"id"`
	Start        time.Time `json:"start"`
	StatusCode   int       `json:"status_code"`
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	Client       string    `json:"client"`
	Stream       bool      `json:"stream"`
	Usage        Usage     `json:"usage"`
	DurationMs   float64   `json:"duration_ms"`
	TTFTMs       *float64  `json:"ttft_ms"`
	Retries      int       `json:"retries"`
	ErrorType    string    `json:"error_type,omitempty"`
	QueueWaitMs  int64     `json:"queue_wait_ms"`
	Conversation string    `json:"conversation_id,omitempty"`
}

type RequestPage struct {
	Records  []RequestRecord `json:"records"`
	More     bool            `json:"more"`
	CursorMs *float64        `json:"cursor_ms"`
	CursorID string          `json:"cursor_id"`
}

type ConfigView struct {
	// Document is the shared /admin/config owner. Raw is the same document as
	// a JSON map for the redactor and the --json output.
	Document *mcp.ConfigGetOutput
	Raw      map[string]any
	Fields   []config.Field
}

func decodeConfig(document *mcp.ConfigGetOutput) (ConfigView, error) {
	view := ConfigView{Document: document}
	if err := remarshal(document.Fields, &view.Fields); err != nil {
		return ConfigView{}, err
	}
	var raw map[string]any
	if err := remarshal(document, &raw); err != nil {
		return ConfigView{}, err
	}
	view.Raw = raw
	return view, nil
}

func (v ConfigView) field(key string) (config.Field, bool) {
	for _, field := range v.Fields {
		if field.Key == key {
			return field, true
		}
	}
	return config.Field{}, false
}

func decodeJSON(raw []byte, to any) error { return json.Unmarshal(raw, to) }

func remarshal(from, to any) error {
	body, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, to)
}
