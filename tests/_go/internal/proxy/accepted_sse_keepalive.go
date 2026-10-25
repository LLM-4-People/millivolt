package proxy

// Round 5 E3 regression: an accepted (202/204/205) event-stream body commits
// through applyStatus, which must classify the body mode from the copied
// Content-Type so the keepalive ticker runs. A JSON accepted body stays quiet.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAcceptedSSEKeepsAlive(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		status      int
		wantComment bool
	}{
		{name: "202 sse", contentType: "text/event-stream", status: http.StatusAccepted, wantComment: true},
		{name: "202 json", contentType: "application/json", status: http.StatusAccepted, wantComment: false},
		{name: "200 sse", contentType: "text/event-stream", status: http.StatusOK, wantComment: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			p := newSSEPacer(rec, 10*time.Millisecond, 0)
			p.Arm()
			hdr := http.Header{"Content-Type": []string{tc.contentType}}
			if tc.status == http.StatusOK {
				p.applyUpstream(hdr)
				if !p.commitStatus(tc.status) {
					t.Fatal("commitStatus did not commit")
				}
			} else if !p.applyStatus(hdr, tc.status) {
				t.Fatal("applyStatus did not commit")
			}
			time.Sleep(40 * time.Millisecond)
			p.Stop()
			if got := strings.Contains(rec.Body.String(), ": keepalive"); got != tc.wantComment {
				t.Fatalf("keepalive = %v, want %v (body %q)", got, tc.wantComment, rec.Body.String())
			}
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
		})
	}
}
