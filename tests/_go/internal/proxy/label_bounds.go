package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// hostileLabelBytes is one MiB, the inbound server header allowance a client
// can spend on a single label today.
const hostileLabelBytes = 1 << 20

// hostileUTF8Label is a client-controlled value one MiB long with a multi-byte
// rune, so a bound that cuts at a byte offset would leave invalid UTF-8.
func hostileUTF8Label() string { return strings.Repeat("é", hostileLabelBytes/2) }

func assertBoundedLabel(t *testing.T, field, got string, max int) {
	t.Helper()
	if len(got) > max {
		t.Fatalf("%s retained %d bytes, want <= %d", field, len(got), max)
	}
	if !strings.HasSuffix(got, retainedTruncationSuffix) {
		t.Fatalf("%s of %d bytes does not end with the retained marker", field, len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("%s of %d bytes is not valid UTF-8", field, len(got))
	}
}

// TestRequestLabelOwnersBoundHostileValues pins each label producer's own
// retention bound: every caller of classifyClient, remoteIP and clientLang sees
// the bounded value, not only the final record assignment.
func TestRequestLabelOwnersBoundHostileValues(t *testing.T) {
	long := hostileUTF8Label()
	t.Run("explicit client label", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set(hdrClient, long)
		assertBoundedLabel(t, "client", classifyClient(r), retainedClientMax)
	})
	t.Run("client from user agent", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("User-Agent", long)
		assertBoundedLabel(t, "client", classifyClient(r), retainedClientMax)
	})
	t.Run("first forwarded address", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("X-Forwarded-For", long+", 203.0.113.9")
		assertBoundedLabel(t, "client IP", remoteIP(r), retainedClientIPMax)
	})
	t.Run("x-stainless-lang", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("X-Stainless-Lang", long)
		assertBoundedLabel(t, "client language", clientLang(r), retainedClientMax)
	})
	t.Run("language from user agent", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("User-Agent", long+"/1.0")
		assertBoundedLabel(t, "client language", clientLang(r), retainedClientMax)
	})
}

// TestRequestLabelOwnersPreserveShortLabels pins byte-identical behavior for
// normal labels: classification precedence, the first forwarded address, and
// the stainless/user-agent language fallbacks are unchanged, and a value at its
// bound is never marked.
func TestRequestLabelOwnersPreserveShortLabels(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("User-Agent", "opencode/1.18.25 ai-sdk/4.0.23")
	r.Header.Set(hdrClient, "opencode")
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	r.Header.Set("X-Stainless-Lang", "python")
	r.Header.Set("X-Stainless-Package-Version", "1.55")
	if got := classifyClient(r); got != "opencode/1.18.25" {
		t.Fatalf("classifyClient = %q, want the fuller user-agent identity", got)
	}
	if got := remoteIP(r); got != "203.0.113.7" {
		t.Fatalf("remoteIP = %q, want the first forwarded value", got)
	}
	if got := clientLang(r); got != "python" {
		t.Fatalf("clientLang = %q, want the explicit stainless language", got)
	}

	fallback := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	fallback.Header.Set("User-Agent", "openai-python/1.55.0")
	fallback.RemoteAddr = "203.0.113.7:4567"
	if got := classifyClient(fallback); got != "openai-python/1.55.0" {
		t.Fatalf("classifyClient fallback = %q", got)
	}
	if got := clientLang(fallback); got != "openai-python" {
		t.Fatalf("clientLang fallback = %q", got)
	}
	if got := remoteIP(fallback); got != "203.0.113.7" {
		t.Fatalf("remoteIP without forwarding = %q", got)
	}

	atClient := strings.Repeat("c", retainedClientMax)
	atIP := strings.Repeat("1", retainedClientIPMax)
	atLang := strings.Repeat("l", retainedClientMax)
	bounded := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	bounded.Header.Set(hdrClient, atClient)
	bounded.Header.Set("X-Forwarded-For", atIP)
	bounded.Header.Set("X-Stainless-Lang", atLang)
	if got := classifyClient(bounded); got != atClient {
		t.Fatalf("at-bound client changed: len=%d", len(got))
	}
	if got := remoteIP(bounded); got != atIP {
		t.Fatalf("at-bound client IP changed: len=%d", len(got))
	}
	if got := clientLang(bounded); got != atLang {
		t.Fatalf("at-bound client language changed: len=%d", len(got))
	}
}

func labelFixtureTransport(t *testing.T) *http.Client {
	t.Helper()
	const response = `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(response)),
			Request:    r,
		}, nil
	})}
}

func serveLabelFixture(t *testing.T, p *Server, body string, configure func(*http.Request)) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set(hdrBaseURL, "http://fixture.example")
	r.Header.Set("Authorization", "Bearer fixture-key")
	configure(r)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body = %s", w.Code, w.Body.String())
	}
}

// TestHostileRequestLabelsBoundedInRingAndDurableRow drives the real request
// path so the finalized ring record and the durable SQLite row prove the
// bounded values, not just the helper returns.
func TestHostileRequestLabelsBoundedInRingAndDurableRow(t *testing.T) {
	p, buf, store := storedProxyForTest(t)
	p.client.Store(labelFixtureTransport(t))
	long := hostileUTF8Label()
	serveLabelFixture(t, p, `{"model":"fixture","messages":[{"role":"user","content":"hi"}]}`, func(r *http.Request) {
		r.Header.Set("User-Agent", long)
		r.Header.Set(hdrClient, long)
		r.Header.Set("X-Forwarded-For", long+", 203.0.113.9")
		r.Header.Set("X-Stainless-Lang", long)
	})
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	ring := waitForRecord(t, buf, 1)[0]
	durable, err := store.LoadRecent(t.Context(), 10)
	if err != nil || len(durable) != 1 {
		t.Fatalf("durable rows = %d err = %v, want one", len(durable), err)
	}
	for _, target := range []struct {
		name string
		rec  *metrics.Record
	}{
		{"ring", ring},
		{"durable", durable[0]},
	} {
		assertBoundedLabel(t, target.name+" user agent", target.rec.UserAgent, retainedUserAgentMax)
		assertBoundedLabel(t, target.name+" client", target.rec.Client, retainedClientMax)
		assertBoundedLabel(t, target.name+" client IP", target.rec.ClientIP, retainedClientIPMax)
		assertBoundedLabel(t, target.name+" client language", target.rec.ClientLang, retainedClientMax)
	}
	if ring.UserAgent != durable[0].UserAgent || ring.Client != durable[0].Client ||
		ring.ClientIP != durable[0].ClientIP || ring.ClientLang != durable[0].ClientLang {
		t.Fatalf("ring and durable labels diverged:\nring=%+v\ndurable=%+v", ring, durable[0])
	}
}

// TestBoundedClientDrivesConversationGrouping proves the bounded classification
// is the identity every consumer sees: two hostile labels that differ only past
// the bound share one conversation partition, and neither the ring nor the
// durable row retains the full header.
func TestBoundedClientDrivesConversationGrouping(t *testing.T) {
	p, buf, store := storedProxyForTest(t)
	p.client.Store(labelFixtureTransport(t))
	prefix := strings.Repeat("a", retainedClientMax)
	labels := []string{prefix + "-first", prefix + "-second"}
	bodies := []string{
		`{"model":"fixture","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"fixture","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"},{"role":"user","content":"again"}]}`,
	}
	for i := range labels {
		label := labels[i]
		serveLabelFixture(t, p, bodies[i], func(r *http.Request) {
			r.Header.Set(hdrClient, label)
		})
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	ring := waitForRecord(t, buf, 2)
	if ring[0].ConversationID == "" || ring[0].ConversationID != ring[1].ConversationID {
		t.Fatalf("bounded clients did not share a conversation: %q vs %q", ring[0].ConversationID, ring[1].ConversationID)
	}
	durable, err := store.LoadRecent(t.Context(), 10)
	if err != nil || len(durable) != 2 {
		t.Fatalf("durable rows = %d err = %v, want two", len(durable), err)
	}
	if durable[0].ConversationID != durable[1].ConversationID || durable[0].ConversationID != ring[0].ConversationID {
		t.Fatalf("durable grouping diverged: ring=%q durable=%q/%q", ring[0].ConversationID, durable[0].ConversationID, durable[1].ConversationID)
	}
	for i, rec := range append(append([]*metrics.Record{}, ring...), durable...) {
		assertBoundedLabel(t, fmt.Sprintf("record %d client", i), rec.Client, retainedClientMax)
		if rec.Client == labels[0] || rec.Client == labels[1] {
			t.Fatalf("record %d retained the full client header", i)
		}
	}
}
