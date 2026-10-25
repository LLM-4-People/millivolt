package proxy

// Round 13 D1 regression: the documented default auth header is also a legal
// explicit selector in either case, normalized to its wire spelling. Every
// other control header stays rejected, and the raw authorization name cannot
// be smuggled through the extra-headers map.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func authSelectorUpstream(t *testing.T) (*httptest.Server, *atomic.Int32, func() []string) {
	t.Helper()
	var calls atomic.Int32
	auth := make(chan []string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		auth <- r.Header.Values("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	return upstream, &calls, func() []string {
		select {
		case got := <-auth:
			return got
		default:
			return nil
		}
	}
}

func TestAuthHeaderSelectorAcceptsDocumentedDefault(t *testing.T) {
	for _, spelling := range []string{"", "Authorization", "authorization", "AUTHORIZATION"} {
		t.Run(spelling, func(t *testing.T) {
			upstream, calls, upstreamAuth := authSelectorUpstream(t)
			defer upstream.Close()
			srv := httptest.NewServer(New(config.Default(), metrics.Noop{}))
			defer srv.Close()

			req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
				strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer sk-test")
			req.Header.Set("X-Proxy-Base-URL", upstream.URL)
			req.Header.Set("Content-Type", "application/json")
			if spelling != "" {
				req.Header.Set(hdrAuthHeader, spelling)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 for selector %q", resp.StatusCode, spelling)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("upstream calls = %d, want 1", got)
			}
			if got := upstreamAuth(); len(got) != 1 || got[0] != "Bearer sk-test" {
				t.Fatalf("upstream Authorization = %v, want one Bearer sk-test", got)
			}
		})
	}

	for _, spelling := range []string{"Cookie", "X-Proxy-Key", "X-Proxy-Base-URL"} {
		t.Run("reject "+spelling, func(t *testing.T) {
			upstream, calls, _ := authSelectorUpstream(t)
			defer upstream.Close()
			srv := httptest.NewServer(New(config.Default(), metrics.Noop{}))
			defer srv.Close()

			req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
				strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer sk-test")
			req.Header.Set("X-Proxy-Base-URL", upstream.URL)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(hdrAuthHeader, spelling)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for selector %q", resp.StatusCode, spelling)
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("rejected selector reached the upstream %d times", got)
			}
		})
	}

	t.Run("authorization injection map stays rejected", func(t *testing.T) {
		upstream, calls, _ := authSelectorUpstream(t)
		defer upstream.Close()
		srv := httptest.NewServer(New(config.Default(), metrics.Noop{}))
		defer srv.Close()

		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set(hdrKey, "sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(hdrHeaders, `{"authorization":["Bearer injected"]}`)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for an authorization injection map", resp.StatusCode)
		}
		if got := calls.Load(); got != 0 {
			t.Fatalf("injection map reached the upstream %d times", got)
		}
	})
}
