package proxy

// Regression tests for the two transport-scoped settings added with the
// buffer settings: the shared upstream header cap (which hot-reloads because
// Reload swaps the client) and the Cursor HTTP/2 transport, which builds its
// own transport and previously ignored the cap.

import (
	"net/http"
	"testing"

	"golang.org/x/net/http2"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestUpstreamResponseHeaderMaxAppliesOnReload(t *testing.T) {
	cfg := config.Default()
	cfg.UpstreamResponseHeaderMax = 4 << 10
	s := New(cfg, metrics.Noop{})
	transport := func() *http.Transport {
		t.Helper()
		client := s.client.Load()
		if client == nil {
			t.Fatal("upstream client is nil")
		}
		tr, ok := client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("transport = %T, want *http.Transport", client.Transport)
		}
		return tr
	}
	if got := transport().MaxResponseHeaderBytes; got != 4<<10 {
		t.Fatalf("initial header cap = %d, want 4096", got)
	}
	next := cfg.Clone()
	next.UpstreamResponseHeaderMax = 8 << 10
	s.Reload(next)
	if got := transport().MaxResponseHeaderBytes; got != 8<<10 {
		t.Fatalf("reloaded header cap = %d, want 8192 (the client must be rebuilt)", got)
	}
}

func TestCursorTransportUsesHeaderCap(t *testing.T) {
	cfg := config.Default()
	cfg.UpstreamResponseHeaderMax = 8 << 10
	s := New(cfg, metrics.Noop{})
	client := s.cursorClientFor()
	ct, ok := client.Transport.(*cursorSchemeTransport)
	if !ok {
		t.Fatalf("cursor transport = %T, want *cursorSchemeTransport", client.Transport)
	}
	for name, tr := range map[string]*http2.Transport{"plain": ct.plain, "tls": ct.tls} {
		if tr.MaxHeaderListSize != 8<<10 {
			t.Errorf("%s cursor transport header cap = %d, want 8192", name, tr.MaxHeaderListSize)
		}
	}
}
