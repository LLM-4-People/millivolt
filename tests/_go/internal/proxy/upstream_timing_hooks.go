package proxy

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestUpstreamTimingHooksByScheme pins the trimmed hook set: the TLS handshake
// pair is installed only for a target whose scheme can actually handshake.
// Installing it for a plaintext upstream would allocate two closures that can
// never fire; omitting it for https would lose the handshake timing.
func TestUpstreamTimingHooksByScheme(t *testing.T) {
	cases := []struct {
		base    string
		tlsWant bool
	}{
		{"http://upstream.example", false},
		{"https://upstream.example", true},
		{"https://upstream.example:8443/base", true},
		// url.Parse lowercases the scheme, so a client-supplied uppercase
		// spelling reaches the same TLS transport and must install the hooks.
		{"HTTPS://upstream.example", true},
	}
	for _, c := range cases {
		tgt := &target{baseURL: c.base}
		if got := tgt.tlsPossible(); got != c.tlsWant {
			t.Fatalf("%s: tlsPossible = %v, want %v", c.base, got, c.tlsWant)
		}
		var timing upstreamTiming
		trace := timing.trace(tgt.tlsPossible())
		if trace == nil {
			t.Fatalf("%s: trace returned nil", c.base)
		}
		if trace.ConnectStart == nil || trace.ConnectDone == nil ||
			trace.GotConn == nil || trace.GotFirstResponseByte == nil {
			t.Errorf("%s: transport hooks must always be installed", c.base)
		}
		if installed := trace.TLSHandshakeStart != nil && trace.TLSHandshakeDone != nil; installed != c.tlsWant {
			t.Errorf("%s: TLS hooks installed = %v, want %v", c.base, installed, c.tlsWant)
		}
	}
}

// TestUpstreamTLSTraceWithUppercaseBaseURL drives a real TLS handshake: the
// client's X-Proxy-Base-URL spelling is HTTPS://, while url.Parse reports the
// scheme as https. The trace hooks must follow the parsed scheme, not a
// case-sensitive prefix, or a real handshake records UpstreamTLSMs as absent.
func TestUpstreamTLSTraceWithUppercaseBaseURL(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"}}]}`)
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(16)
	s := New(config.Default(), buf)
	s.client.Store(upstream.Client())
	base := strings.ToUpper(upstream.URL[:5]) + upstream.URL[5:]

	req := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer fixture")
	req.Header.Set("X-Proxy-Base-URL", base)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	records := waitForRecord(t, buf, 1)
	if len(records) != 1 {
		t.Fatalf("recorded %d records, want 1", len(records))
	}
	if records[0].UpstreamTLSMs <= 0 {
		t.Fatalf("UpstreamTLSMs = %d, want > 0 for a real TLS handshake (base %q)",
			records[0].UpstreamTLSMs, base)
	}
}

// TestUpstreamTimingAbsentHooksRenderNotObserved is the documented zero state.
// "Tracing disabled" means no hook ever fired: adopt must render every field as
// absent (0/false) rather than inventing a duration. The same holds when only
// some hooks fired - a pooled connection has no dial, so connect stays 0 while
// the header wait is observed, and a plaintext target never reports TLS.
func TestUpstreamTimingAbsentHooksRenderNotObserved(t *testing.T) {
	t.Run("no hooks", func(t *testing.T) {
		var timing upstreamTiming
		var rec metrics.Record
		timing.adopt(&rec)
		if rec.UpstreamConnectMs != 0 || rec.UpstreamTLSMs != 0 || rec.UpstreamTTFBMs != 0 || rec.UpstreamConnReused {
			t.Fatalf("adopt with no observations = connect %d, tls %d, ttfb %d, reused %v; want all absent",
				rec.UpstreamConnectMs, rec.UpstreamTLSMs, rec.UpstreamTTFBMs, rec.UpstreamConnReused)
		}
	})

	t.Run("header wait only", func(t *testing.T) {
		timing := upstreamTiming{start: time.Now()}
		trace := timing.trace(false)
		trace.GotFirstResponseByte()
		var rec metrics.Record
		timing.adopt(&rec)
		if rec.UpstreamTTFBMs < 1 {
			t.Fatalf("UpstreamTTFBMs = %d, want observed (>= 1) after GotFirstResponseByte", rec.UpstreamTTFBMs)
		}
		if rec.UpstreamConnectMs != 0 || rec.UpstreamTLSMs != 0 || rec.UpstreamConnReused {
			t.Fatalf("unexpected transport observation: connect %d, tls %d, reused %v",
				rec.UpstreamConnectMs, rec.UpstreamTLSMs, rec.UpstreamConnReused)
		}
	})

	t.Run("plaintext dial never reports tls", func(t *testing.T) {
		timing := upstreamTiming{start: time.Now()}
		trace := timing.trace(false)
		trace.ConnectStart("tcp", "upstream.example:80")
		trace.ConnectDone("tcp", "upstream.example:80", nil)
		trace.GotFirstResponseByte()
		var rec metrics.Record
		timing.adopt(&rec)
		if rec.UpstreamConnectMs < 1 {
			t.Fatalf("UpstreamConnectMs = %d, want observed (>= 1)", rec.UpstreamConnectMs)
		}
		if rec.UpstreamTLSMs != 0 {
			t.Fatalf("UpstreamTLSMs = %d, want 0 for a plaintext target", rec.UpstreamTLSMs)
		}
	})

	t.Run("tls pair reports handshake", func(t *testing.T) {
		timing := upstreamTiming{start: time.Now()}
		trace := timing.trace(true)
		trace.TLSHandshakeStart()
		trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
		var rec metrics.Record
		timing.adopt(&rec)
		if rec.UpstreamTLSMs < 1 {
			t.Fatalf("UpstreamTLSMs = %d, want observed (>= 1)", rec.UpstreamTLSMs)
		}
	})
}
