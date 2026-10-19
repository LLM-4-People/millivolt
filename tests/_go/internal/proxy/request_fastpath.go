package proxy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// requestFastPathCorpus is the differential corpus for the lexical common-shape
// path. It covers the simple accepted shapes plus every routing/parameter
// collection, content array family, duplicate/escaped key, Unicode and invalid
// UTF-8 spelling, and malformed document that must stay on the typed decode.
func requestFastPathCorpus() []struct {
	name string
	body string
	fast bool
} {
	simple := `{"model":"fixture-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	return []struct {
		name string
		body string
		fast bool
	}{
		{"empty object", `{}`, false},
		{"empty object whitespace", " \t\r\n{ } \n", false},
		{"benchmark shape", `{"model":"fixture-model","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("x", 4096) + `"}]}`, true},
		{"model only", `{"model":"m"}`, true},
		{"stream only false", `{"stream":false}`, true},
		{"stream true", `{"stream":true}`, true},
		{"system string", `{"system":"rules","messages":[{"role":"user","content":"hi"}]}`, true},
		{"system null", `{"system":null,"messages":[{"role":"user","content":"hi"}]}`, true},
		{"anthropic minimal", `{"model":"claude","system":"rules","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"}]}`, true},
		{"empty messages", `{"messages":[]}`, true},
		{"empty message", `{"messages":[{}]}`, true},
		{"role only", `{"messages":[{"role":"user"}]}`, true},
		{"content only", `{"messages":[{"content":"ignored"}]}`, true},
		{"content null", `{"messages":[{"role":"user","content":null}]}`, true},
		{"empty strings", `{"model":"","system":"","messages":[{"role":"","content":""}]}`, true},
		{"all roles", `{"messages":[{"role":"system","content":"s"},{"role":"developer","content":"d"},{"role":"user","content":"u"},{"role":"assistant","content":"a"},{"role":"tool","content":"t"},{"role":"function","content":"f"}]}`, true},
		{"last role stays on empty role", `{"messages":[{"role":"user","content":"u"},{"role":"","content":"ignored"}]}`, true},
		{"message order reversed", `{"messages":[{"content":"one","role":"user"}],"system":"s","stream":false,"model":"m"}`, true},
		{"pretty printed", "{\n\t\"model\" : \"m\" ,\n\t\"stream\" : false ,\n\t\"messages\" : [\n\t\t{ \"role\" : \"user\" , \"content\" : \"hi\" }\n\t]\n}", true},
		{"messages before system", `{"messages":[{"role":"user","content":"hi"}],"system":"rules"}`, true},
		{"messages before tools", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`, false},
		{"messages before temperature", `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`, false},
		{"long model", `{"model":"` + strings.Repeat("m", 900) + `"}`, true},
		{"long content", `{"messages":[{"role":"user","content":"` + strings.Repeat("x", 1<<20) + `"}]}`, true},
		{"content controls rejected", `{"messages":[{"role":"user","content":"a` + "\n" + `b"}]}`, false},
		{"content DEL rejected", `{"messages":[{"role":"user","content":"a` + "\x7f" + `b"}]}`, false},
		{"content high byte", `{"messages":[{"role":"user","content":"hé😀"}]}`, false},
		{"content escaped", `{"messages":[{"role":"user","content":"line\nquote\"slash\\"}]}`, false},
		{"content escaped unicode", `{"messages":[{"role":"user","content":"\u0061\u00e9"}]}`, false},
		{"content array text", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`, false},
		{"content array parts", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}},{"type":"input_audio","input_audio":{"data":"AA=="}},{"type":"document","source":{"data":"AA=="}},{"type":"thinking","thinking":"hidden"},{"type":"tool_use","id":"x","name":"f","input":{}},{"type":"tool_result","content":"x"}]}]}`, false},
		{"messages null", `{"messages":null}`, false},
		{"message null element", `{"messages":[null]}`, false},
		{"message duplicate role", `{"messages":[{"role":"user","role":"assistant","content":"hi"}]}`, false},
		{"message duplicate content", `{"messages":[{"role":"user","content":"a","content":"b"}]}`, false},
		{"message unknown key", `{"messages":[{"role":"user","content":"hi","name":"n"}]}`, false},
		{"message tool calls", `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`, false},
		{"messages wrong type", `{"messages":"x"}`, false},
		{"model null", `{"model":null}`, false},
		{"model number", `{"model":7}`, false},
		{"model object", `{"model":{}}`, false},
		{"model escaped", `{"model":"a\u0062"}`, false},
		{"model unicode", `{"model":"hé😀"}`, false},
		{"stream null", `{"stream":null}`, false},
		{"stream string", `{"stream":"true"}`, false},
		{"system array", `{"system":[{"type":"text","text":"rules"}]}`, false},
		{"system number", `{"system":7}`, false},
		{"duplicate model", `{"model":"a","model":"b"}`, false},
		{"duplicate stream", `{"stream":true,"stream":false}`, false},
		{"duplicate messages", `{"messages":[],"messages":[{"role":"user","content":"hi"}]}`, false},
		{"duplicate system", `{"system":"a","system":"b"}`, false},
		{"escaped key", `{"mess\u0061ges":[{"role":"user","content":"hi"}]}`, false},
		{"escaped model key", `{"mod\u0065l":"m"}`, false},
		{"case folded model key", `{"MODEL":"m"}`, false},
		{"case folded messages key", `{"Messages":[]}`, false},
		{"unknown key", `{"model":"m","custom":1}`, false},
		{"trailing document", simple + ` {}`, false},
		{"root array", `[]`, false},
		{"root null", `null`, false},
		{"root string", `"x"`, false},
		{"truncated", `{"model":"m","messages":[{"role":"user","content":"hi"}`, false},
		{"trailing comma", `{"model":"m",}`, false},
		{"collective tool schema", `{"model":"m","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`, false},
		{"tools wrong type", `{"model":"m","tools":{}}`, false},
		{"tool choice", `{"model":"m","tool_choice":"auto"}`, false},
		{"max tokens", `{"model":"m","max_tokens":7}`, false},
		{"max completion tokens", `{"model":"m","max_completion_tokens":7}`, false},
		{"temperature", `{"model":"m","temperature":0.5}`, false},
		{"top p", `{"model":"m","top_p":0.9}`, false},
		{"n", `{"model":"m","n":2}`, false},
		{"stream options", `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`, false},
		{"stop string", `{"model":"m","stop":"END"}`, false},
		{"stop array", `{"model":"m","stop":["a","b"]}`, false},
		{"logprobs", `{"model":"m","logprobs":true}`, false},
		{"presence penalty", `{"model":"m","presence_penalty":0.1}`, false},
		{"frequency penalty", `{"model":"m","frequency_penalty":0.1}`, false},
		{"response format", `{"model":"m","response_format":{"type":"json_object"}}`, false},
		{"seed", `{"model":"m","seed":42}`, false},
		{"parallel tools", `{"model":"m","parallel_tool_calls":false}`, false},
		{"logit bias", `{"model":"m","logit_bias":{"1":-1}}`, false},
		{"top logprobs", `{"model":"m","top_logprobs":3}`, false},
		{"service tier", `{"model":"m","service_tier":"flex"}`, false},
		{"thinking", `{"model":"m","thinking":{"type":"enabled","budget_tokens":1000}}`, false},
		{"metadata", `{"model":"m","metadata":{"a":"b"}}`, false},
		{"reasoning effort", `{"model":"m","reasoning_effort":"high"}`, false},
		{"verbosity", `{"model":"m","verbosity":"low"}`, false},
		{"reasoning", `{"model":"m","reasoning":{"effort":"xhigh"}}`, false},
		{"responses input", `{"model":"m","input":"hello","max_output_tokens":7}`, false},
		{"invalid utf8 small", string([]byte(`{"messages":[{"role":"user","content":"a`)) + "\xff\xfe" + string([]byte(`"}]}`)), false},
	}
}

func compareFastPathRecord(t *testing.T, name string, body []byte, preview bool) {
	t.Helper()
	var got, want metrics.Record
	gotErr := parseLLMRequest(body, &got, preview)
	wantErr := parseLLMRequestSlow(body, &want, preview)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("%s preview=%v: error mismatch: fast=%v slow=%v", name, preview, gotErr, wantErr)
	}
	if gotErr != nil && gotErr.Error() != wantErr.Error() {
		t.Fatalf("%s preview=%v: error text mismatch: fast=%q slow=%q", name, preview, gotErr, wantErr)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s preview=%v: record mismatch:\nfast=%+v\nslow=%+v", name, preview, got, want)
	}
	if preview {
		return
	}
	var direct, decoded metrics.Record
	if parseLLMRequestFast(body, &direct) {
		decodedErr := parseLLMRequestSlow(body, &decoded, false)
		if decodedErr != nil {
			t.Fatalf("%s: fast path accepted a body the typed decode rejects: %v", name, decodedErr)
		}
		if !reflect.DeepEqual(direct, decoded) {
			t.Fatalf("%s: direct fast record mismatch:\nfast=%+v\nslow=%+v", name, direct, decoded)
		}
	}
}

func TestRequestMetadataFastPathDifferential(t *testing.T) {
	for _, tc := range requestFastPathCorpus() {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			for _, preview := range []bool{false, true} {
				compareFastPathRecord(t, tc.name, body, preview)
			}
			var direct metrics.Record
			if got := parseLLMRequestFast(body, &direct); got != tc.fast {
				t.Fatalf("parseLLMRequestFast = %v, want %v", got, tc.fast)
			}
		})
	}
}

// TestRequestMetadataFastPathMessageBound keeps the lexical element count on
// the same rejection boundary as the typed preflight: the cap is accepted and
// the next element falls back to the existing over-cap rejection.
func TestRequestMetadataFastPathMessageBound(t *testing.T) {
	messages := func(n int) []byte {
		var b strings.Builder
		b.WriteString(`{"messages":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{}`)
		}
		b.WriteString(`]}`)
		return []byte(b.String())
	}
	for _, tc := range []struct {
		name string
		n    int
		fast bool
	}{
		{"at cap", requestMetadataMaxElements, true},
		{"over cap", requestMetadataMaxElements + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := messages(tc.n)
			var direct metrics.Record
			if got := parseLLMRequestFast(body, &direct); got != tc.fast {
				t.Fatalf("parseLLMRequestFast = %v, want %v", got, tc.fast)
			}
			compareFastPathRecord(t, tc.name, body, false)
		})
	}
}

// TestRequestMetadataFastPathCoversBenchmarkShapes pins that the benchmark
// bodies actually take the lexical path rather than silently falling back.
func TestRequestMetadataFastPathCoversBenchmarkShapes(t *testing.T) {
	for _, size := range []int{1024, 100 * 1024, 1024 * 1024} {
		body := []byte(benchmarkRequestBody(size))
		var rec metrics.Record
		if !parseLLMRequestFast(body, &rec) {
			t.Fatalf("bytes%d benchmark body fell back to the typed decode", size)
		}
		if rec.CharsUser != size || rec.Model != "fixture-model" || !rec.Stream || rec.TurnsUser != 1 {
			t.Fatalf("bytes%d fast record = %+v", size, rec)
		}
	}
}

func FuzzRequestFastPathEquivalence(f *testing.F) {
	for _, tc := range requestFastPathCorpus() {
		f.Add([]byte(tc.body))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 1<<20 {
			t.Skip()
		}
		compareFastPathRecord(t, "fuzz", body, false)
	})
}
