package main

// Wire types for millivolt's operator API. Every decoder is tolerant: millivolt
// is pre-1.0 and its payloads grow fields between releases, so unknown keys are
// ignored and nullable numbers are pointers.

import (
	"encoding/json"
	"strconv"
)

type KPI struct {
	Requests        float64  `json:"requests"`
	Errors          float64  `json:"errors"`
	InFlight        float64  `json:"in_flight"`
	Cost            float64  `json:"cost"`
	CostPerReq      float64  `json:"cost_per_req"`
	CostPerMTok     float64  `json:"cost_per_mtok"`
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

type Counters struct {
	InFlight      float64 `json:"in_flight"`
	TotalRequests float64 `json:"total_requests"`
	TotalErrors   float64 `json:"total_errors"`
}

type PauseState struct {
	Paused           bool             `json:"paused"`
	Queued           float64          `json:"queued"`
	Holds            []map[string]any `json:"holds"`
	KnownClients     []string         `json:"known_clients"`
	KnownProviders   []string         `json:"known_providers"`
	DefaultMaxQueued float64          `json:"default_max_queued"`
}

type ThrottleState struct {
	Active         bool             `json:"active"`
	Throttles      []map[string]any `json:"throttles"`
	KnownProviders []string         `json:"known_providers"`
}

type DebugState struct {
	Enabled  bool             `json:"enabled"`
	Sessions []map[string]any `json:"sessions"`
}

type Snapshot struct {
	KPI              KPI            `json:"kpi"`
	Storage          StorageState   `json:"storage"`
	Pause            PauseState     `json:"pause"`
	Throttle         ThrottleState  `json:"throttle"`
	Debug            DebugState     `json:"debug"`
	DashboardVersion string         `json:"dashboard_version"`
	Counters         Counters       `json:"counters"`
	Raw              map[string]any `json:"-"`
}

func DecodeSnapshot(body []byte) (Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(body, &s); err != nil {
		return s, err
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err == nil {
		s.Raw = raw
	}
	return s, nil
}

type RequestRow struct {
	ID           string
	Start        string
	Status       int
	Provider     string
	Model        string
	Client       string
	Stream       bool
	DurationMs   float64
	TTFTMs       *float64
	InputTokens  float64
	OutputTokens float64
	Retries      float64
	ErrorType    string
	Raw          map[string]any
}

// UnmarshalJSON handles the nested usage object and the two duration spellings
// millivolt has used, plus a missing error_type (errors are status-based).
func (r *RequestRow) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.Raw = raw
	r.ID = str(raw["id"])
	r.Start = str(raw["start"])
	r.Status = int(toFloat(raw["status_code"]))
	r.Provider = str(raw["provider"])
	r.Model = str(raw["model"])
	r.Client = str(raw["client"])
	r.Stream = truthy(raw["stream"])
	r.DurationMs = toFloat(raw["duration_ms"])
	if r.DurationMs == 0 {
		r.DurationMs = toFloat(raw["processing_ms"])
	}
	if v, ok := raw["ttft_ms"]; ok && v != nil {
		n := toFloat(v)
		r.TTFTMs = &n
	}
	usage := asMap(raw["usage"])
	r.InputTokens = toFloat(usage["input_tokens"])
	r.OutputTokens = toFloat(usage["output_tokens"])
	r.Retries = toFloat(raw["retries"])
	r.ErrorType = str(raw["error_type"])
	return nil
}

type RequestPage struct {
	Rows     []RequestRow   `json:"records"`
	More     bool           `json:"more"`
	CursorMs *float64       `json:"cursor_ms"`
	CursorID string         `json:"cursor_id"`
	Raw      map[string]any `json:"-"`
}

func DecodeRequestPage(body []byte) (RequestPage, error) {
	var p RequestPage
	if err := json.Unmarshal(body, &p); err != nil {
		return p, err
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err == nil {
		p.Raw = raw
	}
	return p, nil
}

type ExplorerGroup struct {
	Name   string  `json:"name"`
	N      float64 `json:"n"`
	Cost   float64 `json:"cost"`
	In     float64 `json:"in"`
	Out    float64 `json:"out"`
	Cache  float64 `json:"cache"`
	Reason float64 `json:"reason"`
	Err    float64 `json:"err"`
}

type Explorer struct {
	Dim        string          `json:"dim"`
	Total      float64         `json:"total"`
	ErrorTotal float64         `json:"error_total"`
	Groups     []ExplorerGroup `json:"groups"`
}

type ChartBucket struct {
	T     float64 `json:"t"`
	Req   float64 `json:"req"`
	Err   float64 `json:"err"`
	RL    float64 `json:"rl"`
	In    float64 `json:"in"`
	Out   float64 `json:"out"`
	Cache float64 `json:"cache"`
	Cost  float64 `json:"cost"`
}

type Chart struct {
	NowMs    float64       `json:"now_ms"`
	FromMs   float64       `json:"from_ms"`
	BucketMs float64       `json:"bucket_ms"`
	Buckets  []ChartBucket `json:"buckets"`
}

type ConfigDoc struct {
	Values          map[string]any   `json:"values"`
	Effective       map[string]any   `json:"effective"`
	Defaults        map[string]any   `json:"defaults"`
	Revision        string           `json:"revision"`
	RestartRequired []string         `json:"restart_required"`
	Fields          []map[string]any `json:"fields"`
	Categories      []map[string]any `json:"categories"`
	Overrides       map[string]any   `json:"overrides"`
	Writable        bool             `json:"writable"`
	Path            string           `json:"path"`
}

// Small tolerant JSON helpers shared by every decoder.
func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	case string:
		if f, err := strconv.ParseFloat(x, 64); err == nil {
			return f
		}
	}
	return 0
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func truthy(v any) bool {
	b, _ := v.(bool)
	return b
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
