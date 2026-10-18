package metrics

import (
	"net/http"
	"testing"
)

// TestSetStreamHeadersPreservesNoStore pins the shared SSE header owner against
// a caller that already staged a stricter Cache-Control. The operator gate
// answers a Bearer-authenticated request with no-store and a session cookie;
// the stream handler must not downgrade that to no-cache. A plain stream still
// gets no-cache.
func TestSetStreamHeadersPreservesNoStore(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing []string
		want     string
	}{
		{"plain stream", nil, "no-cache"},
		{"no-store", []string{"no-store"}, "no-store"},
		{"no-store directive list", []string{"no-cache, No-Store"}, "no-cache, No-Store"},
		{"other cache policy", []string{"private"}, "no-cache"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for _, value := range tc.existing {
				h.Add("Cache-Control", value)
			}
			SetStreamHeaders(h)
			if got := h.Get("Cache-Control"); got != tc.want {
				t.Fatalf("Cache-Control = %q, want %q", got, tc.want)
			}
			if got := h.Get("Content-Type"); got != "text/event-stream" {
				t.Fatalf("Content-Type = %q, want text/event-stream", got)
			}
			if got := h.Get("Connection"); got != "keep-alive" {
				t.Fatalf("Connection = %q, want keep-alive", got)
			}
		})
	}
}
