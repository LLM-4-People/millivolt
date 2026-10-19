package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestRequestMetadataRoutingAndTypeBoundaries pins the split-decode
// contract. On a parameters-block type error the early return recovers
// exactly ONE field - the token cap (max_tokens / max_completion_tokens,
// the value the ServeHTTP trust-boundary check reads) - while every other
// parameter stays dropped (the temperature probe row) and routing still
// survives independently. Malformed JSON never decodes anything.
func TestRequestMetadataRoutingAndTypeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body, model string
		stream, cap       bool
	}{
		{"valid", `{"model":"m","stream":true,"max_tokens":7,"messages":[{"role":"user","content":"hi"}]}`, "m", true, true},
		{"metadata type error", `{"tools":{},"model":"m","stream":true,"max_tokens":7}`, "m", true, true},
		{"late metadata type error", `{"model":"m","stream":true,"max_tokens":7,"tools":{}}`, "m", true, true},
		{"routing type error", `{"model":{},"stream":true,"max_tokens":7}`, "", false, true},
		{"late routing type error", `{"max_tokens":7,"stream":"yes","model":"m"}`, "", false, true},
		{"syntax error", `{"model":"m","stream":true,"max_tokens":7,`, "", false, false},
		{"trailing document", `{"model":"m","stream":true,"max_tokens":7} {}`, "", false, false},
		{"array root", `[{"model":"m","stream":true,"max_tokens":7}]`, "", false, false},
		{"null root", `null`, "", false, false},
		{"case insensitive duplicate", `{"MODEL":"old","model":"m","STREAM":false,"Stream":true,"MAX_TOKENS":3,"max_tokens":7}`, "m", true, true},
		{"null routing duplicate", `{"model":"m","model":null,"stream":true,"stream":null,"max_tokens":7}`, "m", true, true},
		{"overflow in tools", `{"model":"m","stream":true,"max_tokens":7,"tools":[{"nested":1e999}]}`, "m", true, true},
		{"overflow in metadata", `{"model":"m","stream":true,"max_tokens":7,"metadata":{"x":1e999}}`, "m", true, true},
		{"overflow in logit bias", `{"model":"m","stream":true,"max_tokens":7,"logit_bias":{"x":` + strings.Repeat("9", 309) + `}}`, "m", true, true},
		{"finite scientific value", `{"model":"m","stream":true,"max_tokens":7,"tools":[1e99,"1e999"]}`, "m", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec metrics.Record
			parseLLMRequest([]byte(tc.body), &rec, false)
			if rec.Model != tc.model || rec.Stream != tc.stream {
				t.Fatalf("routing = %q/%v, want %q/%v", rec.Model, rec.Stream, tc.model, tc.stream)
			}
			if (rec.ReqMaxTokens != nil) != tc.cap || (tc.cap && *rec.ReqMaxTokens != 7) {
				t.Fatalf("max_tokens = %v, cap recovered=%v", rec.ReqMaxTokens, tc.cap)
			}
		})
	}

	// The cap is the ONLY parameter the split path recovers: a type error
	// elsewhere must not blind the trust-boundary check, but it must also
	// not resurrect the rest of the parameters block.
	var rec metrics.Record
	parseLLMRequest([]byte(`{"model":"m","stream":true,"max_tokens":7,"temperature":0.5,"tools":{}}`), &rec, false)
	if rec.ReqMaxTokens == nil || *rec.ReqMaxTokens != 7 {
		t.Fatalf("split path: max_tokens = %v, want the recovered 7", rec.ReqMaxTokens)
	}
	if rec.ReqTemperature != nil {
		t.Fatalf("split path: temperature = %v, want it dropped with the failed parameters decode", *rec.ReqTemperature)
	}
}

// TestRequestMetadataScalarsBoundedBeforeRing drives the production metadata
// decode with body-sized tool_choice, response_format.type, service_tier and
// message role values, then records through the real retention boundary. The
// full request JSON is never retained; only these scalar metadata fields are
// bounded, and normal values stay byte-identical.
func TestRequestMetadataScalarsBoundedBeforeRing(t *testing.T) {
	const bound = 512
	huge := strings.Repeat("m", 900<<10)
	body, err := json.Marshal(map[string]any{
		"model":           "fixture",
		"tool_choice":     huge,
		"response_format": map[string]any{"type": huge},
		"service_tier":    huge,
		"messages":        []map[string]any{{"role": huge, "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var rec metrics.Record
	parseLLMRequest(body, &rec, false)
	if rec.ReqToolChoice != huge || rec.ReqResponseFormat != huge ||
		rec.ReqServiceTier != huge || rec.LastTurnRole != huge {
		t.Fatalf("production decode did not capture the hostile scalars: %+v", rec)
	}
	b := metrics.NewBuffer(2)
	b.Record(&rec)
	got := b.Snapshot()[0]
	for _, field := range []struct {
		name string
		got  string
	}{
		{"request tool choice", got.ReqToolChoice},
		{"request response format", got.ReqResponseFormat},
		{"request service tier", got.ReqServiceTier},
		{"last turn role", got.LastTurnRole},
	} {
		if len(field.got) != bound {
			t.Fatalf("%s retained %d bytes, want the exact %d-byte bound", field.name, len(field.got), bound)
		}
		if !strings.HasSuffix(field.got, "…") {
			t.Fatalf("%s does not end with the retention marker", field.name)
		}
	}

	var normal metrics.Record
	parseLLMRequest([]byte(`{"model":"fixture","tool_choice":"auto","response_format":{"type":"json_object"},"service_tier":"flex","messages":[{"role":"assistant","content":"hi"}]}`), &normal, false)
	if normal.ReqToolChoice != "auto" || normal.ReqResponseFormat != "json_object" ||
		normal.ReqServiceTier != "flex" || normal.LastTurnRole != "assistant" {
		t.Fatalf("normal metadata decode changed: %+v", normal)
	}
	before := normal
	b.Record(&normal)
	gotNormal := b.Snapshot()[1]
	if !reflect.DeepEqual(*gotNormal, before) {
		t.Fatalf("normal metadata changed at the retention boundary:\nbefore=%+v\nafter=%+v", before, *gotNormal)
	}
}

func TestRequestMetadataDiscardedShapesAndDuplicates(t *testing.T) {
	var rec metrics.Record
	parseLLMRequest([]byte(`{"tools":[{},null,42,"schema",[1,2],false],
		"metadata":{"a":1,"a":2,"b":{"nested":[1,2]}},"metadata":{"c":null},
		"logit_bias":{"a":false,"b":"text"},"logit_bias":{"a":null},
		"messages":[{"role":"user","content":"discarded","content":"kept"},
		{"role":"assistant","content":[{"type":"text","text":"text","text":null}]},
		{"role":"tool","content":[{"type":"text","text":7}]}]}`), &rec, false)
	if rec.ReqToolsCount != 6 || rec.ReqMetadataKeys != 3 || rec.ReqLogitBias != 2 {
		t.Fatalf("shape counts = tools:%d metadata:%d bias:%d", rec.ReqToolsCount, rec.ReqMetadataKeys, rec.ReqLogitBias)
	}
	if rec.CharsUser != 4 || rec.CharsAssistant != 4 || rec.CharsTool != 0 || rec.TurnsTool != 1 {
		t.Fatalf("content/duplicate semantics changed: %+v", rec)
	}
	var nulled metrics.Record
	parseLLMRequest([]byte(`{"tools":[1],"tools":null,"metadata":{"a":1},"metadata":null,
		"system":"old","system":null,"messages":[{"role":"user","content":"old","content":null}]}`), &nulled, false)
	if nulled.ReqToolsCount != 0 || nulled.ReqMetadataKeys != 0 || nulled.CharsSystem != 0 || nulled.CharsUser != 0 {
		t.Fatalf("null replacement retained old metadata: %+v", nulled)
	}
}

// TestRequestToolChoiceRecordsOnlyFunctionName pins the recorded shape of
// tool_choice: the string shorthand is kept verbatim, an object contributes
// only function.name with the "tool:" prefix, and malformed or unrelated
// shapes record nothing. The typed decoder never materializes nested members,
// so these semantics are the whole contract.
func TestRequestToolChoiceRecordsOnlyFunctionName(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"string shorthand", `{"tool_choice":"auto"}`, "auto"},
		{"required shorthand", `{"tool_choice":"required"}`, "required"},
		{"empty shorthand", `{"tool_choice":""}`, ""},
		{"null", `{"tool_choice":null}`, ""},
		{"function object", `{"tool_choice":{"type":"function","function":{"name":"weather"}}}`, "tool:weather"},
		{"empty function name", `{"tool_choice":{"function":{"name":""}}}`, "tool:"},
		{"non-string function name", `{"tool_choice":{"function":{"name":7}}}`, ""},
		{"function is not an object", `{"tool_choice":{"function":"weather"}}`, ""},
		{"function without a name", `{"tool_choice":{"type":"function","function":{}}}`, ""},
		{"null function", `{"tool_choice":{"function":null}}`, ""},
		{"array", `{"tool_choice":[1,2]}`, ""},
		{"number", `{"tool_choice":1}`, ""},
		{"boolean", `{"tool_choice":true}`, ""},
		{"unknown nested members are skipped", `{"tool_choice":{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"x":{"type":"array","items":{"enum":[1,2,3]}}}}}}}`, "tool:f"},
		{"duplicate function last wins", `{"tool_choice":{"function":{"name":"first"},"function":{"name":"second"}}}`, "tool:second"},
		{"duplicate function last valid after malformed", `{"tool_choice":{"function":"bad","function":{"name":"second"}}}`, "tool:second"},
		{"duplicate function last malformed", `{"tool_choice":{"function":{"name":"first"},"function":"bad"}}`, ""},
		{"duplicate tool_choice last wins", `{"tool_choice":"auto","tool_choice":{"function":{"name":"f"}}}`, "tool:f"},
		{"duplicate tool_choice last null", `{"tool_choice":{"function":{"name":"f"}},"tool_choice":null}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec metrics.Record
			parseLLMRequest([]byte(tc.body), &rec, false)
			if rec.ReqToolChoice != tc.want {
				t.Fatalf("ReqToolChoice = %q, want %q", rec.ReqToolChoice, tc.want)
			}
		})
	}
}

// hostileToolChoiceBody builds one MiB of tool_choice payload beside a valid
// function name: the nested shape floods an unknown array, the wide shape
// floods the top-level object, and the function shape floods a member of the
// function object itself. All three are amplification the map decode paid for
// even though only function.name is retained.
func hostileToolChoiceBody(shape string) []byte {
	var body strings.Builder
	body.WriteString(`{"model":"m","tool_choice":{"function":`)
	switch shape {
	case "function":
		body.WriteString(`{"name":"f","x":[`)
	case "nested":
		body.WriteString(`{"name":"f"},"x":[`)
	case "wide":
		body.WriteString(`{"name":"f"},"x":{`)
	}
	for i := 0; body.Len() < 1<<20; i++ {
		if i > 0 {
			body.WriteByte(',')
		}
		if shape == "wide" {
			fmt.Fprintf(&body, `"k%06d":0`, i)
		} else {
			body.WriteString(`{}`)
		}
	}
	switch shape {
	case "function":
		body.WriteString(`]}`) // close the x array and the function object
	case "nested":
		body.WriteString(`]`) // close the x array
	case "wide":
		body.WriteString(`}`) // close the x object
	}
	body.WriteString(`}}`) // close tool_choice and the root object
	return []byte(body.String())
}

// TestRequestToolChoiceDecodeAllocationBounded is the P10 regression: a one
// MiB nested, wide, or function-local tool_choice must cost a small constant
// beyond the request bytes, not a per-node materialization. The former
// map[string]any decode allocated about 60 MiB for the nested fixture.
func TestRequestToolChoiceDecodeAllocationBounded(t *testing.T) {
	for _, name := range []string{"nested", "wide", "function"} {
		t.Run(name, func(t *testing.T) {
			body := hostileToolChoiceBody(name)
			var warm metrics.Record
			parseLLMRequest(body, &warm, false)
			if warm.ReqToolChoice != "tool:f" {
				t.Fatalf("warm decode = %q, want tool:f", warm.ReqToolChoice)
			}
			res := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					var rec metrics.Record
					parseLLMRequest(body, &rec, false)
					if rec.ReqToolChoice != "tool:f" {
						b.Fatalf("ReqToolChoice = %q, want tool:f", rec.ReqToolChoice)
					}
				}
			})
			if alloc := res.AllocedBytesPerOp(); alloc > 4<<20 {
				t.Fatalf("%d-byte tool_choice allocated %d bytes/op, want <= 4 MiB", len(body), alloc)
			} else {
				t.Logf("%s tool_choice: %d bytes, %d bytes/op", name, len(body), alloc)
			}
		})
	}
}

// TestRequestMetadataKeyCountBounded pins the metadata key-count contract:
// counts below the cap are exact and preserve map-merge/null semantics, while
// a key flood saturates at requestMetadataKeysMax without materializing a
// per-key value map.
func TestRequestMetadataKeyCountBounded(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"normal keys", `{"metadata":{"a":1,"b":2,"c":3}}`, 3},
		{"empty object", `{"metadata":{}}`, 0},
		{"duplicate metadata objects merge", `{"metadata":{"a":1},"metadata":{"a":2,"b":3}}`, 2},
		{"null clears the accumulated keys", `{"metadata":{"a":1},"metadata":null}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec metrics.Record
			parseLLMRequest([]byte(tc.body), &rec, false)
			if rec.ReqMetadataKeys != tc.want {
				t.Fatalf("ReqMetadataKeys = %d, want %d", rec.ReqMetadataKeys, tc.want)
			}
		})
	}

	var body strings.Builder
	body.WriteString(`{"model":"m","metadata":{`)
	for i := 0; body.Len() < 1<<20; i++ {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `"k%06d":0`, i)
	}
	body.WriteString(`}}`)
	raw := []byte(body.String())
	var rec metrics.Record
	parseLLMRequest(raw, &rec, false)
	if rec.ReqMetadataKeys != requestMetadataKeysMax {
		t.Fatalf("hostile metadata count = %d, want the %d-key cap", rec.ReqMetadataKeys, requestMetadataKeysMax)
	}
	res := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			var rec metrics.Record
			parseLLMRequest(raw, &rec, false)
			if rec.ReqMetadataKeys != requestMetadataKeysMax {
				b.Fatalf("ReqMetadataKeys = %d, want the cap", rec.ReqMetadataKeys)
			}
		}
	})
	if alloc := res.AllocedBytesPerOp(); alloc > 4<<20 {
		t.Fatalf("%d-byte metadata map allocated %d bytes/op, want <= 4 MiB", len(raw), alloc)
	} else {
		t.Logf("metadata flood: %d bytes, %d bytes/op, %d allocs/op", len(raw), alloc, res.AllocsPerOp())
	}
	// The former map decode made one allocation per key (about 87k for this
	// fixture); a counting scan with a bounded key set stays far below that.
	if allocs := res.AllocsPerOp(); allocs > 10_000 {
		t.Fatalf("%d-byte metadata map made %d allocs/op, want a bounded key scan", len(raw), allocs)
	}
}

// TestRequestMetadataTypeErrorKeepsCapOnly pins the split-decode boundary for
// a non-object metadata value: the routing fields and the token cap survive,
// while the rest of the parameters block stays dropped.
func TestRequestMetadataTypeErrorKeepsCapOnly(t *testing.T) {
	var rec metrics.Record
	parseLLMRequest([]byte(`{"model":"m","stream":true,"max_tokens":7,"temperature":0.5,"metadata":"x"}`), &rec, false)
	if rec.Model != "m" || !rec.Stream || rec.ReqMaxTokens == nil || *rec.ReqMaxTokens != 7 {
		t.Fatalf("routing/cap lost on a metadata type error: %+v", rec)
	}
	if rec.ReqTemperature != nil {
		t.Fatalf("temperature = %v, want it dropped with the failed parameters decode", *rec.ReqTemperature)
	}
}

func TestRequestMetadataDuplicateMessageArrayReuse(t *testing.T) {
	for _, tc := range []struct {
		name, suffix string
		wantTurns    int
	}{
		{"omitted fields retain reused slot", `,"messages":[{}]`, 1},
		{"null retains reused slot", `,"messages":[null]`, 1},
		{"empty array discards reused slots", `,"messages":[],"messages":[{}]`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec metrics.Record
			parseLLMRequest([]byte(`{"messages":[{"role":"user","content":"kept"}]`+tc.suffix+`}`), &rec, true)
			wantPreview := ""
			if tc.wantTurns > 0 {
				wantPreview = "kept"
			}
			if rec.TurnsUser != tc.wantTurns || rec.CharsUser != 4*tc.wantTurns || rec.PromptPreview != wantPreview {
				t.Fatalf("reused array semantics changed: turns=%d chars=%d preview=%q", rec.TurnsUser, rec.CharsUser, rec.PromptPreview)
			}
		})
	}
}

func TestRequestMetadataUnicodeContentAndPreview(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`"ASCII"`), []byte(`"hé😀"`), []byte(`"line\n\r\t\b\f\"\\\/end"`),
		[]byte(`"\u0061\u00e9\ud83d\ude00"`), []byte(`"\ud800x\udc00"`),
		{'"', 'a', 0xff, 0xfe, '"'},
		[]byte(`"` + strings.Repeat("é", 100) + `"`),
		[]byte(`"` + strings.Repeat(`\u00e9`, 100) + `"`),
	} {
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		body := append([]byte(`{"messages":[{"role":"user","content":`), raw...)
		body = append(body, []byte(`}]}`)...)
		for _, preview := range []bool{false, true} {
			var rec metrics.Record
			parseLLMRequest(body, &rec, preview)
			wantPreview := ""
			if preview {
				wantPreview = metrics.TruncatePreview(decoded)
			}
			if rec.CharsUser != len(decoded) || rec.PromptPreview != wantPreview {
				t.Fatalf("raw=%q preview=%v: chars=%d/%d preview=%q/%q", raw, preview, rec.CharsUser, len(decoded), rec.PromptPreview, wantPreview)
			}
		}
	}
	var rec metrics.Record
	parseLLMRequest([]byte(`{"messages":[{"role":"user","content":"earlier"},
		{"role":"user","content":[{"type":"text","text":"latest"},{"type":"input_image","image_url":"data:image/png;base64,AA=="},
		{"type":"input_audio","input_audio":{"data":"AA=="}}]}]}`), &rec, true)
	if rec.PromptPreview != "" || rec.CharsUser != 13 || rec.Images != 1 || rec.Attachments != 1 {
		t.Fatalf("multimodal last-user preview/shape = %+v", rec)
	}
}

func FuzzRequestStringSize(f *testing.F) {
	for _, seed := range []string{"", "plain ASCII", "hé😀", "line\n\r\t\b\f\"\\/end", "literal \\u0061", string([]byte{'a', 0xff, 0xfe}), "\u2028\u2029"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		var got requestStringSize
		if err := got.UnmarshalJSON(raw); err != nil {
			t.Fatal(err)
		}
		if int(got) != len(decoded) {
			t.Fatalf("raw=%q: byte count=%d, decoded length=%d", raw, got, len(decoded))
		}
	})
}

type requestTestBody struct {
	io.Reader
	closed bool
}

func (r *requestTestBody) Close() error { r.closed = true; return nil }

type requestErrorReader struct{ err error }

func (r requestErrorReader) Read([]byte) (int, error) { return 0, r.err }

type requestTimedReader struct {
	io.Reader
	lastRead time.Time
}

func (r *requestTimedReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.lastRead = time.Now()
	return n, err
}

func TestReadRequestOwnsPostUploadStart(t *testing.T) {
	const body = `{"messages":[{"role":"user","content":"hi"}]}`
	r := &requestTimedReader{Reader: strings.NewReader(body)}
	_, rec, err := readRequest(&http.Request{Body: io.NopCloser(r), ContentLength: int64(len(body))}, 1024, false)
	if err != nil || rec.Start.IsZero() || rec.Start.Before(r.lastRead) || rec.Start.After(time.Now()) || rec.CharsUser != 2 {
		t.Fatalf("Start must follow upload and precede returning parsed metadata: rec=%+v lastRead=%v err=%v", rec, r.lastRead, err)
	}
	_, empty, err := readRequest(&http.Request{}, 1024, false)
	if err != nil || empty.Start.IsZero() {
		t.Fatalf("body-less request must share the timing owner: rec=%+v err=%v", empty, err)
	}
	var pure metrics.Record
	parseLLMRequest([]byte(body), &pure, false)
	if !pure.Start.IsZero() {
		t.Fatal("the pure metadata decoder must not own request timing")
	}
}

func TestReadRequestBoundedLengthHintAndErrors(t *testing.T) {
	const body = ` {"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]} `
	for _, declared := range []int64{-1, 0, 1, int64(len(body)), int64(len(body) + 50), 1 << 50} {
		rc := &requestTestBody{Reader: strings.NewReader(body)}
		r := &http.Request{Body: rc, ContentLength: declared}
		got, rec, err := readRequest(r, 32<<20, false)
		if err != nil || string(got) != body || rec.Model != "m" || !rec.Stream || !rc.closed {
			t.Fatalf("length=%d: body=%q record=%+v closed=%v err=%v", declared, got, rec, rc.closed, err)
		}
		if declared > requestReadHintMax && cap(got) > requestReadHintMax {
			t.Fatalf("untrusted content length allocated capacity %d", cap(got))
		}
	}
	for _, size := range []int{0, 1, 1023, 1024, 1025} {
		for _, declared := range []int64{-1, 1, 1 << 50} {
			rc := &requestTestBody{Reader: strings.NewReader(strings.Repeat("x", size))}
			got, _, err := readRequest(&http.Request{Body: rc, ContentLength: declared}, 1024, false)
			if (err != nil) != (size > 1024) || (err == nil && len(got) != size) || !rc.closed {
				t.Fatalf("size=%d length=%d: len=%d closed=%v err=%v", size, declared, len(got), rc.closed, err)
			}
			if size > 1024 {
				var overflow *http.MaxBytesError
				if !errors.As(err, &overflow) || overflow.Limit != 1024 {
					t.Fatalf("body overflow lost its typed limit: %T %v", err, err)
				}
			}
		}
	}
	for _, readErr := range []error{io.ErrUnexpectedEOF, errors.New("read failed")} {
		for _, declared := range []int64{-1, 100} {
			rc := &requestTestBody{Reader: requestErrorReader{readErr}}
			_, _, err := readRequest(&http.Request{Body: rc, ContentLength: declared}, 1024, false)
			if !errors.Is(err, readErr) || !rc.closed {
				t.Fatalf("reader error changed: got=%v want=%v closed=%v", err, readErr, rc.closed)
			}
		}
	}
	got, rec, err := readRequest(&http.Request{}, 1024, false)
	if err != nil || len(got) != 0 || rec == nil {
		t.Fatalf("nil body: %q %+v %v", got, rec, err)
	}
}

// TestReadRequestHintAvoidsLargeBodyCopy pins the proxy caller's admitted-size
// hint: a declared 2 MiB body must allocate about one body, not the 1 MiB
// prefix plus a full right-sized copy.
func TestReadRequestHintAvoidsLargeBodyCopy(t *testing.T) {
	body := strings.Repeat("x", 2<<20)
	res := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			rc := &requestTestBody{Reader: strings.NewReader(body)}
			got, _, err := readRequestHint(&http.Request{Body: rc, ContentLength: int64(len(body))}, 32<<20, false, int64(len(body)))
			if err != nil || len(got) != len(body) {
				b.Fatalf("len=%d err=%v", len(got), err)
			}
		}
	})
	if alloc := res.AllocedBytesPerOp(); alloc > int64(len(body))*3/2 {
		t.Fatalf("2 MiB hint allocated %d bytes/op, want about one body (prefix+copy regression)", alloc)
	}
}

func TestProxyRequestBodyLimitHTTP(t *testing.T) {
	for _, size := range []int{1023, 1024, 1025} {
		for _, declared := range []int64{-1, 0, 1, 1 << 50} {
			cfg := config.Default()
			cfg.MaxRequestBytes, cfg.MaxRetries, cfg.QualityRetries = 1024, 0, 0
			buffer := metrics.NewBuffer(1)
			server := New(cfg, buffer)
			calls := 0
			body := strings.Repeat("x", size) // Invalid JSON remains transparent below the cap.
			server.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				got, err := io.ReadAll(r.Body)
				if err != nil || string(got) != body {
					t.Fatalf("accepted request body changed: bytes=%d err=%v", len(got), err)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"fixture"},"finish_reason":"stop"}]}`)), Request: r}, nil
			})})
			reader := &requestTestBody{Reader: strings.NewReader(body)}
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", reader)
			request.ContentLength = declared
			request.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			wantStatus, wantRecords := http.StatusOK, 1
			if size > 1024 {
				wantStatus, wantRecords = http.StatusRequestEntityTooLarge, 0
			}
			if response.Code != wantStatus || calls != wantRecords || buffer.Len() != wantRecords || !reader.closed {
				t.Errorf("size=%d length=%d: status=%d calls=%d records=%d closed=%v", size, declared, response.Code, calls, buffer.Len(), reader.closed)
			}
		}
	}
	// Ordinary upload failures remain 400, without a retry, upstream call or
	// finalized inference record. Only typed body-limit errors become 413.
	cfg := config.Default()
	buffer := metrics.NewBuffer(1)
	server := New(cfg, buffer)
	server.client.Store(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("failed upload reached upstream")
		return nil, nil
	})})
	reader := &requestTestBody{Reader: requestErrorReader{io.ErrUnexpectedEOF}}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", reader)
	request.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || buffer.Len() != 0 || !reader.closed {
		t.Fatalf("failed upload: status=%d records=%d closed=%v", response.Code, buffer.Len(), reader.closed)
	}
}

func TestRequestMetadataTranslatedBodyOwnership(t *testing.T) {
	cfg := config.Default()
	buf := metrics.NewBuffer(10)
	s := New(cfg, buf)
	var upstream []byte
	s.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstream, _ = io.ReadAll(r.Body)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id":"response-1","model":"fixture-model","role":"assistant","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":1}}`))}, nil
	})})
	body := `{"model":"fixture-model","stream":false,"max_completion_tokens":50,"metadata":{"ignored":1},
		"messages":[{"role":"system","content":"system rules"},{"role":"user","content":"hello"}]}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set(hdrBaseURL, "http://fixture.example")
	r.Header.Set(hdrFormat, "anthropic")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	want, err := translateRequest("anthropic", []byte(body), cfg.AnthropicDefaultMaxTokens, "")
	if err != nil || !bytes.Equal(upstream, want) || w.Code != http.StatusOK {
		t.Fatalf("translation changed: code=%d upstream=%s want=%s err=%v", w.Code, upstream, want, err)
	}
	var expected metrics.Record
	parseLLMRequest(want, &expected, false)
	got := buf.Snapshot()[0]
	if got.Model != "fixture-model" || got.Stream || !reflect.DeepEqual(got.ReqMaxTokens, expected.ReqMaxTokens) ||
		got.CharsSystem != expected.CharsSystem || got.CharsUser != expected.CharsUser ||
		got.ReqMetadataKeys != expected.ReqMetadataKeys || got.ReqMetadataKeys != 0 {
		t.Fatalf("metadata must describe translated body: got=%+v expected=%+v", got, expected)
	}
}

// TestMaxCompletionTokensPrecedence pins the merge order in parseLLMRequest:
// when a body carries BOTH max_tokens and max_completion_tokens, the decode
// takes max_completion_tokens. The merge matches the Anthropic translator's
// pick (TranslateRequest writes max_completion_tokens into the translated
// max_tokens), so the trust-boundary hostileTokenCap check sees exactly the
// field a translated body actually carries - a flipped merge would let a
// decoy max_tokens hide a hostile max_completion_tokens from the bound (and
// vice versa against the translator). Nothing else discriminated this order:
// the bound rows use one field at a time.
func TestMaxCompletionTokensPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int // 0 means nil
	}{
		{"both fields: max_completion_tokens wins", `{"model":"m","max_tokens":100,"max_completion_tokens":200}`, 200},
		{"max_tokens only", `{"model":"m","max_tokens":100}`, 100},
		{"max_completion_tokens only", `{"model":"m","max_completion_tokens":200}`, 200},
		{"null controls: both present but null decode to nil", `{"model":"m","max_tokens":null,"max_completion_tokens":null}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec metrics.Record
			parseLLMRequest([]byte(tc.body), &rec, false)
			if tc.want == 0 {
				if rec.ReqMaxTokens != nil {
					t.Fatalf("ReqMaxTokens = %v, want nil", *rec.ReqMaxTokens)
				}
				return
			}
			if rec.ReqMaxTokens == nil || *rec.ReqMaxTokens != tc.want {
				t.Fatalf("ReqMaxTokens = %v, want %d", rec.ReqMaxTokens, tc.want)
			}
		})
	}
}

// TestClassifyClientLabelPrecedence pins the recorded client identity. An
// explicit X-Proxy-Client label wins outright, except when it merely repeats
// the user agent's product name while the agent reports more detail - the
// fuller user-agent identity is then the true one, so one app through several
// provider blocks records one identity. Everything else keeps the
// established precedence: the label, then x-stainless-lang, then the user
// agent's product token.
func TestClassifyClientLabelPrecedence(t *testing.T) {
	cases := []struct {
		name                 string
		label, ua, lang, pkg string
		want                 string
	}{
		{name: "coarse label keeps the fuller user-agent identity", label: "opencode", ua: "opencode/latest/2.0.26/cli ai-sdk/4.0.23", want: "opencode/latest/2.0.26/cli"},
		{name: "coarse label keeps a plain versioned identity", label: "opencode", ua: "opencode/1.18.25", want: "opencode/1.18.25"},
		{name: "a distinct label is an explicit override", label: "my-team", ua: "opencode/1.18.25", want: "my-team"},
		{name: "a richer label is an explicit override", label: "opencode/zen", ua: "opencode/1.18.25", want: "opencode/zen"},
		{name: "a label beside an unrelated user agent wins", label: "opencode", ua: "curl/8.14.1", want: "opencode"},
		{name: "a label without a user agent wins", label: "opencode", want: "opencode"},
		{name: "a label equal to the product token stays", label: "opencode/1.18.25", ua: "opencode/1.18.25", want: "opencode/1.18.25"},
		{name: "the user agent's product token fills in without a label", ua: "opencode/latest/2.0.26/cli ai-sdk/4.0.23", want: "opencode/latest/2.0.26/cli"},
		{name: "x-stainless-lang beats user-agent inference", ua: "openai-python/1.55.0", lang: "python", pkg: "1.55", want: "python 1.55"},
		{name: "x-stainless-lang without a package version stands alone", lang: "go", want: "go"},
		{name: "a label beats x-stainless-lang", label: "my-team", lang: "python", want: "my-team"},
		{name: "nothing classifies", want: "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if tc.label != "" {
				req.Header.Set(hdrClient, tc.label)
			}
			if tc.ua != "" {
				req.Header.Set("User-Agent", tc.ua)
			}
			if tc.lang != "" {
				req.Header.Set("X-Stainless-Lang", tc.lang)
			}
			if tc.pkg != "" {
				req.Header.Set("X-Stainless-Package-Version", tc.pkg)
			}
			if got := classifyClient(req); got != tc.want {
				t.Fatalf("classifyClient(label=%q ua=%q) = %q, want %q", tc.label, tc.ua, got, tc.want)
			}
		})
	}
}

func TestRequestCollectionElementBounds(t *testing.T) {
	array := func(n int, elem string) string {
		if n == 0 {
			return "[]"
		}
		return "[" + strings.TrimSuffix(strings.Repeat(elem+",", n), ",") + "]"
	}
	objectRange := func(start, n int) string {
		var b strings.Builder
		b.WriteByte('{')
		for i := start; i < start+n; i++ {
			if i > start {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"k%d":0`, i)
		}
		b.WriteByte('}')
		return b.String()
	}
	object := func(n int) string { return objectRange(0, n) }
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"messages at cap", `{"model":"m","messages":` + array(requestMetadataMaxElements, `{}`) + `}`, false},
		{"messages over cap", `{"model":"m","messages":` + array(requestMetadataMaxElements+1, `{}`) + `}`, true},
		{"tools over cap", `{"model":"m","tools":` + array(requestMetadataMaxElements+1, `{}`) + `}`, true},
		{"logit_bias over cap", `{"model":"m","logit_bias":` + object(requestMetadataMaxElements+1) + `}`, true},
		{"escaped messages key", `{"mess\u0061ges":` + array(requestMetadataMaxElements+1, `{}`) + `}`, true},
		{"case-folded messages key", `{"Messages":` + array(requestMetadataMaxElements+1, `{}`) + `}`, true},
		{"escaped case-folded messages key", `{"\u004dESSAGES":` + array(requestMetadataMaxElements+1, `{}`) + `}`, true},
		{"case-folded logit_bias key", `{"LOGIT_BIAS":` + object(requestMetadataMaxElements+1) + `}`, true},
		{"separator key is not a messages field", `{"Mes-sages":` + array(requestMetadataMaxElements+1, `{}`) + `}`, false},
		{"duplicate logit_bias objects accumulate", `{"logit_bias":` + objectRange(0, requestMetadataMaxElements/2+1) + `,"logit_bias":` + objectRange(requestMetadataMaxElements/2+1, requestMetadataMaxElements/2+1) + `}`, true},
		{"duplicate logit_bias objects under cap", `{"logit_bias":` + objectRange(0, 4000) + `,"logit_bias":` + objectRange(4000, 4000) + `}`, false},
		{"duplicate logit_bias members count once", `{"logit_bias":{"a":0` + strings.Repeat(`,"a":0`, requestMetadataMaxElements) + `}}`, false},
		{"truncated logit_bias passes through", `{"model":"m","logit_bias":{"a":`, false},
		{"null logit_bias resets duplicate objects", `{"logit_bias":` + objectRange(0, 6000) + `,"logit_bias":null,"logit_bias":` + objectRange(6000, 6000) + `}`, false},
		{"non-array messages is not an element collection", `{"model":"m","messages":"not-an-array"}`, false},
		{"malformed JSON stays passthrough", `{"model":"m","messages":[`, false},
		{"malformed over-cap prefix stays passthrough", `{"model":"m","messages":` + array(requestMetadataMaxElements+1, `{}`) + `,X}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec metrics.Record
			err := parseLLMRequest([]byte(tc.body), &rec, false)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseLLMRequest error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestRequestElementOverflowReturns400BeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[]}`))
	}))
	defer upstream.Close()
	srv := httptest.NewServer(New(config.Default(), metrics.Noop{}))
	defer srv.Close()

	var b strings.Builder
	b.WriteString(`{"model":"m","messages":[`)
	for i := 0; i <= requestMetadataMaxElements; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{}`)
	}
	b.WriteString(`]}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(b.String()))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", resp.StatusCode, raw)
	}
	if !bytes.Contains(raw, []byte("exceeds")) || !bytes.Contains(raw, []byte("invalid_request_error")) {
		t.Fatalf("body = %q, want bounded invalid_request_error", raw)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("upstream called %d times, want 0", got)
	}
}

func TestRequestNestedPayloadAllocationBound(t *testing.T) {
	nested := strings.TrimSuffix(strings.Repeat("{} ,", 200_000), ",")
	for _, tc := range []struct {
		name string
		body string
	}{
		{"content parts", `{"model":"m","messages":[{"role":"user","content":[` + nested + `]}]}`},
		{"tool payload", `{"model":"m","tools":[{"nested":[` + nested + `],"n":1e999}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.body)
			var parseErr error
			res := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					var rec metrics.Record
					parseErr = parseLLMRequest(raw, &rec, false)
					if parseErr != nil {
						b.Fatalf("parse returned %v", parseErr)
					}
				}
			})
			if alloc := res.AllocedBytesPerOp(); alloc > 4<<20 {
				t.Fatalf("%d-byte %s allocated %d bytes/op, want bounded nested decoding", len(raw), tc.name, alloc)
			} else {
				t.Logf("%s: %d bytes, %d bytes/op", tc.name, len(raw), alloc)
			}
		})
	}
}
