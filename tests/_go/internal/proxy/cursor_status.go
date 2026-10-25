package proxy

// Round 11 E2 regression: a non-200 cursor Run response never carries a
// completion. 202/204/205 relay the status and body verbatim; 304 and other
// non-200 statuses fail closed through the transport sink instead of
// synthesizing SSE under a non-completion status.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func cursorFixedStatusUpstream(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	h2s := &http2.Server{}
	up := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(status)
		if body != "" && status != http.StatusNoContent &&
			status != http.StatusResetContent && status != http.StatusNotModified {
			w.Write([]byte(body))
		}
	}), h2s))
	up.EnableHTTP2 = true
	up.Start()
	return up
}

func TestCursorStreamNon200Statuses(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		upstreamBody string
		wantStatus   int
		wantContains string
		wantEmpty    bool
		wantError    bool
	}{
		{name: "202 relays the acknowledgement", status: http.StatusAccepted, upstreamBody: "accepted", wantStatus: http.StatusAccepted, wantContains: "accepted"},
		{name: "204 relays an empty success", status: http.StatusNoContent, wantStatus: http.StatusNoContent, wantEmpty: true},
		{name: "205 relays an empty reset", status: http.StatusResetContent, wantStatus: http.StatusResetContent, wantEmpty: true},
		{name: "304 fails closed", status: http.StatusNotModified, wantStatus: http.StatusBadGateway, wantContains: "upstream_unreachable", wantError: true},
		{name: "302 fails closed", status: http.StatusFound, upstreamBody: "redirect", wantStatus: http.StatusBadGateway, wantContains: "upstream_unreachable", wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := cursorFixedStatusUpstream(t, tc.status, tc.upstreamBody)
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
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", resp.StatusCode, tc.wantStatus, body)
			}
			if tc.wantEmpty && len(body) != 0 {
				t.Fatalf("body = %q, want empty", body)
			}
			if tc.wantContains != "" && !strings.Contains(string(body), tc.wantContains) {
				t.Fatalf("body = %q, want %q", body, tc.wantContains)
			}
			if strings.Contains(string(body), "data:") {
				t.Fatalf("non-completion status synthesized SSE: %q", body)
			}

			rec := waitForRecord(t, buf, 1)[0]
			if rec.StatusCode != tc.wantStatus {
				t.Fatalf("record status = %d, want %d (%+v)", rec.StatusCode, tc.wantStatus, rec)
			}
			if rec.ClientDisconnected {
				t.Fatalf("record marked a client disconnect for a status-only response: %+v", rec)
			}
			if tc.wantError {
				if !rec.IsError() || rec.ErrorType != typeUpstreamUnreachable {
					t.Fatalf("record = %+v, want upstream_unreachable", rec)
				}
			} else if rec.IsError() {
				t.Fatalf("record = %+v, want no error for a relayed non-completion status", rec)
			}
		})
	}
}
