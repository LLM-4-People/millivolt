package storage

import "github.com/LLM-4-People/millivolt/internal/metrics"

// Existing rows predate presence tracking. Negative means unknown, preserving
// their historical nonzero-only interpretation; no migration invents lost zeros.
const requestParamPresenceDDL = "req_param_presence INTEGER NOT NULL DEFAULT -1"

type requestParamField struct {
	present func(*metrics.Record) bool
	restore func(*metrics.Record, bool, bool)
}

func optionalRequestParam[T comparable](field func(*metrics.Record) **T) requestParamField {
	return requestParamField{
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
	optionalRequestParam(func(r *metrics.Record) **int { return &r.ReqMaxTokens }),
	optionalRequestParam(func(r *metrics.Record) **float64 { return &r.ReqTemperature }),
	optionalRequestParam(func(r *metrics.Record) **float64 { return &r.ReqTopP }),
	optionalRequestParam(func(r *metrics.Record) **int { return &r.ReqN }),
	optionalRequestParam(func(r *metrics.Record) **float64 { return &r.ReqPresencePen }),
	optionalRequestParam(func(r *metrics.Record) **float64 { return &r.ReqFrequencyPen }),
	optionalRequestParam(func(r *metrics.Record) **int64 { return &r.ReqSeed }),
	optionalRequestParam(func(r *metrics.Record) **bool { return &r.ReqParallelTools }),
	optionalRequestParam(func(r *metrics.Record) **int { return &r.ReqTopLogprobs }),
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
