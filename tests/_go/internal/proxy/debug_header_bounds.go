package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestDebugCaptureSkipsOverlongHeaderNames drives the real debug-capture sidecar
// path: a credential-bearing header name far past the retained-name cap is
// dropped rather than stored, while the fixed redaction marker for an ordinary
// credential header and the map total budget stay intact.
func TestDebugCaptureSkipsOverlongHeaderNames(t *testing.T) {
	spy := &liveSpy{}
	p := newStoredTestProxy(t, spy)
	startDebugSession(t, p)

	hostile := "X-" + strings.Repeat("d", 600<<10) + "-token"
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[]}`))
	})
	rec := runDebugCaptureRequest(t, p, spy, upstream, `{"model":"m","messages":[]}`, http.Header{
		hostile:         {"leak"},
		"Authorization": {"Bearer sk-secret"},
	})
	raw := fetchDebugCapture(t, p, rec.ID)

	var doc struct {
		Request struct {
			Headers []harNV `json:"headers"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("capture doc: %v", err)
	}
	total := 0
	for _, h := range doc.Request.Headers {
		if len(h.Name) > retainedHeaderNameMax {
			t.Fatalf("capture retained header name of %d bytes, want <= %d", len(h.Name), retainedHeaderNameMax)
		}
		if h.Name == hostile {
			t.Fatal("capture retained the overlong credential header name")
		}
		total += len(h.Name) + len(h.Value)
	}
	if total > retainedHeadersMaxBytes {
		t.Fatalf("retained request header bytes = %d, want <= %d", total, retainedHeadersMaxBytes)
	}
	authorization := ""
	for _, h := range doc.Request.Headers {
		if h.Name == "Authorization" {
			authorization = h.Value
			break
		}
	}
	if authorization != "[REDACTED]" {
		t.Fatalf("Authorization = %q, want [REDACTED]", authorization)
	}
}
