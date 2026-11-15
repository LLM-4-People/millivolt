package proxy

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestRequestLogCarriesTheRecordID pins the correlation contract: the minted
// record id exists before the first request-scoped line and prefixes it, so a
// line can be joined to the stored row. The skip path is used because it is the
// one request-scoped log in the relay: an override rule plus a body that is not
// a JSON object makes the rewrite decline and log.
func TestRequestLogCarriesTheRecordID(t *testing.T) {
	cap := func(v int) *int { return &v }
	up := newFixtureUpstream(t, "application/json", 200,
		`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	cfg := config.Default()
	// Scoped by client, not model: an unparseable body never yields a model.
	cfg.RequestOverrides = []config.RequestOverride{{Client: "fixture", Body: &config.OverrideBody{MaxTokens: cap(9)}}}
	buf := metrics.NewBuffer(10)
	s := New(cfg, buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// Invalid JSON is passed through unchanged by design, so the override
	// cannot decode it and the skip line is written.
	r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions", strings.NewReader(`not json at all`))
	r.Header.Set("Authorization", "Bearer sk-test")
	r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
	r.Header.Set("X-Proxy-Client", "fixture")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
	}
	recs := waitForRecord(t, buf, 1)
	id := recs[0].ID
	if id == "" {
		t.Fatal("record has no id")
	}
	line := logged.String()
	if !strings.Contains(line, "request "+id+": ") {
		t.Fatalf("log line %q does not carry the record id %q", line, id)
	}
	// The id is minted before the rewrite decision, not only at the final
	// wiring: a line written there would otherwise stamp an empty id.
	if strings.Contains(line, "request : ") {
		t.Fatalf("log line carries an empty id: %q", line)
	}
}
