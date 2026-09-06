package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestRequestMetadataRoutingAndTypeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body, model string
		stream, params    bool
	}{
		{"valid", `{"model":"m","stream":true,"max_tokens":7,"messages":[{"role":"user","content":"hi"}]}`, "m", true, true},
		{"metadata type error", `{"tools":{},"model":"m","stream":true,"max_tokens":7}`, "m", true, false},
		{"late metadata type error", `{"model":"m","stream":true,"max_tokens":7,"tools":{}}`, "m", true, false},
		{"routing type error", `{"model":{},"stream":true,"max_tokens":7}`, "", false, true},
		{"late routing type error", `{"max_tokens":7,"stream":"yes","model":"m"}`, "", false, true},
		{"syntax error", `{"model":"m","stream":true,"max_tokens":7,`, "", false, false},
		{"trailing document", `{"model":"m","stream":true,"max_tokens":7} {}`, "", false, false},
		{"array root", `[{"model":"m","stream":true,"max_tokens":7}]`, "", false, false},
		{"null root", `null`, "", false, false},
		{"case insensitive duplicate", `{"MODEL":"old","model":"m","STREAM":false,"Stream":true,"MAX_TOKENS":3,"max_tokens":7}`, "m", true, true},
		{"null routing duplicate", `{"model":"m","model":null,"stream":true,"stream":null,"max_tokens":7}`, "m", true, true},
		{"overflow in tools", `{"model":"m","stream":true,"max_tokens":7,"tools":[{"nested":1e999}]}`, "m", true, false},
		{"overflow in metadata", `{"model":"m","stream":true,"max_tokens":7,"metadata":{"x":1e999}}`, "m", true, false},
		{"overflow in logit bias", `{"model":"m","stream":true,"max_tokens":7,"logit_bias":{"x":` + strings.Repeat("9", 309) + `}}`, "m", true, false},
		{"finite scientific value", `{"model":"m","stream":true,"max_tokens":7,"tools":[1e99,"1e999"]}`, "m", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec metrics.Record
			parseLLMRequest([]byte(tc.body), &rec, false)
			if rec.Model != tc.model || rec.Stream != tc.stream {
				t.Fatalf("routing = %q/%v, want %q/%v", rec.Model, rec.Stream, tc.model, tc.stream)
			}
			if (rec.ReqMaxTokens != nil) != tc.params || (tc.params && *rec.ReqMaxTokens != 7) {
				t.Fatalf("max_tokens = %v, metadata valid=%v", rec.ReqMaxTokens, tc.params)
			}
		})
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
	want, err := translateRequest("anthropic", []byte(body), cfg.AnthropicDefaultMaxTokens)
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
