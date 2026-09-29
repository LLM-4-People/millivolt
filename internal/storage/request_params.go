package storage

import "github.com/LLM-4-People/millivolt/internal/metrics"

// Existing rows predate presence tracking. Negative means unknown, preserving
// their historical nonzero-only interpretation; no migration invents lost zeros.
const requestParamPresenceDDL = "req_param_presence INTEGER NOT NULL DEFAULT -1"

type requestParamField struct {
	column  string
	present func(*metrics.Record) bool
	restore func(*metrics.Record, bool, bool)
}

func optionalRequestParam[T comparable](column string, field func(*metrics.Record) **T) requestParamField {
	return requestParamField{
		column:  column,
		present: func(r *metrics.Record) bool { return *field(r) != nil },
		restore: func(r *metrics.Record, present, legacy bool) {
			p := field(r)
			var zero T
			if (legacy && *p != nil && **p == zero) || (!legacy && !present) {
				*p = nil
			}
		},
	}
}

// The order is the durable bit mapping: append new pointer fields, never reorder.
// This ONE list owns both write-side presence and read-side nil restoration.
var requestParamFields = [...]requestParamField{
	optionalRequestParam("req_max_tokens", func(r *metrics.Record) **int { return &r.ReqMaxTokens }),
	optionalRequestParam("req_temperature", func(r *metrics.Record) **float64 { return &r.ReqTemperature }),
	optionalRequestParam("req_top_p", func(r *metrics.Record) **float64 { return &r.ReqTopP }),
	optionalRequestParam("req_n", func(r *metrics.Record) **int { return &r.ReqN }),
	optionalRequestParam("req_presence_pen", func(r *metrics.Record) **float64 { return &r.ReqPresencePen }),
	optionalRequestParam("req_frequency_pen", func(r *metrics.Record) **float64 { return &r.ReqFrequencyPen }),
	optionalRequestParam("req_seed", func(r *metrics.Record) **int64 { return &r.ReqSeed }),
	optionalRequestParam("req_parallel_tools", func(r *metrics.Record) **bool { return &r.ReqParallelTools }),
	optionalRequestParam("req_top_logprobs", func(r *metrics.Record) **int { return &r.ReqTopLogprobs }),
}

// RequestParamColumns lists, in durable bit order, the optional request
// parameter COLUMNS whose unset/zero distinction requestParamPresence
// preserves. It is derived from requestParamFields - the same owner as the bit
// mapping and the write/read presence - so a column added there cannot drift
// from what the MCP reference documents, and an external mirror (describe) can
// be pinned against it instead of a second hand-written list.
func RequestParamColumns() []string {
	out := make([]string, len(requestParamFields))
	for i, field := range requestParamFields {
		out[i] = field.column
	}
	return out
}

func requestParamPresence(r *metrics.Record) int64 {
	var mask int64
	for i, field := range requestParamFields {
		if field.present(r) {
			mask |= 1 << i
		}
	}
	return mask
}

func restoreRequestParams(r *metrics.Record, mask int64) {
	for i, field := range requestParamFields {
		field.restore(r, mask&(1<<i) != 0, mask < 0)
	}
}
