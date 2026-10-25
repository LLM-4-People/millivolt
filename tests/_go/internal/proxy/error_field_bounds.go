package proxy

// Round 34 NEW-5 regression: a proxy-captured provider error type/code is
// client-controlled bytes up to the error-body cap. The pre-commit JSON writer
// and the in-band SSE writer must both apply the shared preview bound, so a
// cursor 400 or a committed-stream retry cannot echo a 60 KB classification.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestCursorErrorTypeBounded(t *testing.T) {
	const huge = "provider_" + "T"
	long := huge + strings.Repeat("T", 60_000)
	body := `{"error":{"type":"` + long + `","message":"bad request"}}`
	up := cursorFixedStatusUpstream(t, http.StatusBadRequest, body)
	defer up.Close()
	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(cursorTestCfg(up.URL), buf))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-4.5-sonnet-thinking","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer cursor-key")
	req.Header.Set("X-Proxy-Base-URL", up.URL)
	req.Header.Set("X-Proxy-Format", "cursor")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw := new(bytes.Buffer)
	raw.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", resp.StatusCode, raw)
	}
	if raw.Len() >= 1024 {
		t.Fatalf("client error body = %d bytes, want the bounded envelope", raw.Len())
	}
	if strings.Contains(raw.String(), long[:1024]) {
		t.Fatal("client error body carries the unbounded provider type")
	}
	if !strings.Contains(raw.String(), "bad request") {
		t.Fatalf("client error lost the provider message: %s", raw)
	}
	rec := waitForRecord(t, buf, 1)[0]
	if len(rec.ErrorType) > 512 {
		t.Fatalf("record error type = %d bytes, want the retained bound", len(rec.ErrorType))
	}
}

func TestProtocolErrorSinksBoundProviderFields(t *testing.T) {
	long := strings.Repeat("E", 60_000)
	for _, inbound := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
		t.Run(string(inbound)+"/http", func(t *testing.T) {
			w := httptestResponseWriter()
			writeProtocolHTTPErrorCodeHdr(w, inbound, http.StatusBadRequest, long, long, "bad", nil)
			if w.body.Len() >= 1024 {
				t.Fatalf("HTTP error body = %d bytes, want the bounded envelope", w.body.Len())
			}
			if strings.Contains(w.body.String(), long[:1024]) {
				t.Fatal("HTTP error body carries the unbounded provider tuple")
			}
		})
		t.Run(string(inbound)+"/in-band", func(t *testing.T) {
			var buf bytes.Buffer
			rec := &metrics.Record{}
			if err := writeProtocolStreamError(&buf, rec, inbound, "id", "m", long, long, "bad"); err != nil {
				t.Fatal(err)
			}
			if buf.Len() >= 4096 {
				t.Fatalf("in-band error frames = %d bytes, want the bounded tuple", buf.Len())
			}
			if strings.Contains(buf.String(), long[:1024]) {
				t.Fatal("in-band error frames carry the unbounded provider tuple")
			}
		})
	}
}

// httptestResponseWriter is the minimal buffered writer the pre-commit error
// sink needs; using httptest.ResponseRecorder would also work, but this keeps
// the assertion on the returned body local.
type boundedErrorWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func httptestResponseWriter() *boundedErrorWriter {
	return &boundedErrorWriter{header: http.Header{}}
}

func (w *boundedErrorWriter) Header() http.Header  { return w.header }
func (w *boundedErrorWriter) WriteHeader(code int) { w.status = code }
func (w *boundedErrorWriter) Write(p []byte) (int, error) {
	return w.body.Write(p)
}
