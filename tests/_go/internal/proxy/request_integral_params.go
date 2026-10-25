package proxy

// Round 11 A1 regression: a valid integral spelling in n, seed or top_logprobs
// must not erase the request record. The wire codecs already accept 1.0 and
// 1e1; the proxy's typed record decode used to fail them and skip every
// parameter, composition and reservation field.

import (
	"fmt"
	"strings"
	"testing"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func integralParamsBody(n, seed, topLogprobs string) string {
	return fmt.Sprintf(`{"model":"m","stream":true,"n":%s,"seed":%s,"top_logprobs":%s,`+
		`"temperature":0.7,"tools":[{"type":"function","function":{"name":"f","parameters":{}}}],`+
		`"stop":["a","b"],"metadata":{"k":"v"},`+
		`"messages":[{"role":"system","content":"SYS"},{"role":"user","content":"hello there"}]}`,
		n, seed, topLogprobs)
}

func TestRequestIntegralParamsKeepRecord(t *testing.T) {
	parse := func(body string) metrics.Record {
		t.Helper()
		var rec metrics.Record
		if err := parseLLMRequest([]byte(body), &rec, false); err != nil {
			t.Fatalf("parse: %v", err)
		}
		return rec
	}
	control := parse(integralParamsBody("1", "1", "1"))
	if control.ReqN == nil || *control.ReqN != 1 {
		t.Fatalf("control n = %v, want 1", control.ReqN)
	}
	if control.ReqToolsCount != 1 || control.ReqStop != 2 || control.ReqMetadataKeys != 1 ||
		control.TurnsUser != 1 || control.CharsSystem+control.CharsUser != 14 {
		t.Fatalf("control composition = %+v, want tools=1 stop=2 meta=1 turns=1 chars=14", control)
	}
	controlEstimate := estimateTokens(&control)
	if controlEstimate == 0 {
		t.Fatal("control estimate is zero; the fixture does not exercise admission")
	}

	for _, tc := range []struct {
		spelling string
		want     int
	}{
		{spelling: "1.0", want: 1},
		{spelling: "1e1", want: 10},
	} {
		t.Run(tc.spelling, func(t *testing.T) {
			body := integralParamsBody(tc.spelling, tc.spelling, tc.spelling)
			rec := parse(body)
			if rec.ReqN == nil || *rec.ReqN != tc.want {
				t.Fatalf("n = %v, want %d", rec.ReqN, tc.want)
			}
			if rec.ReqSeed == nil || *rec.ReqSeed != int64(tc.want) {
				t.Fatalf("seed = %v, want %d", rec.ReqSeed, tc.want)
			}
			if rec.ReqTopLogprobs == nil || *rec.ReqTopLogprobs != tc.want {
				t.Fatalf("top_logprobs = %v, want %d", rec.ReqTopLogprobs, tc.want)
			}
			if rec.ReqTemperature == nil || *rec.ReqTemperature != 0.7 {
				t.Fatalf("temperature = %v, want 0.7", rec.ReqTemperature)
			}
			if rec.ReqToolsCount != 1 || rec.ReqStop != 2 || rec.ReqMetadataKeys != 1 ||
				rec.TurnsUser != 1 || rec.CharsSystem+rec.CharsUser != 14 {
				t.Fatalf("composition = %+v, want tools=1 stop=2 meta=1 turns=1 chars=14", rec)
			}
			if got := estimateTokens(&rec); got != controlEstimate {
				t.Fatalf("estimateTokens = %d, want the control %d", got, controlEstimate)
			}

			// The translated wire accepts the same spelling: the record must
			// describe what Chat-to-Responses sends.
			out, _, err := providerformat.TranslateRequestBetween(ir.Chat, ir.Responses, []byte(body), 0, "")
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if want := fmt.Sprintf(`"top_logprobs":%d`, tc.want); !strings.Contains(string(out), want) {
				t.Fatalf("translated body lacks %s: %s", want, out)
			}
		})
	}

	// A fractional value is not an integer and records nothing for that field,
	// but it must not fail the request or erase the rest of the record.
	t.Run("1.5", func(t *testing.T) {
		rec := parse(integralParamsBody("1.5", "1.5", "1.5"))
		if rec.ReqN != nil || rec.ReqSeed != nil || rec.ReqTopLogprobs != nil {
			t.Fatalf("fractional values recorded: n=%v seed=%v top=%v", rec.ReqN, rec.ReqSeed, rec.ReqTopLogprobs)
		}
		if rec.ReqToolsCount != 1 || rec.TurnsUser != 1 || rec.CharsUser != 11 {
			t.Fatalf("composition = %+v, want the other fields preserved", rec)
		}
	})
}
