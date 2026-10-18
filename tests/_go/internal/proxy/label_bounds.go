package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// hostileLabelBytes is one MiB, the inbound server header allowance a client
// can spend on a single label today.
const hostileLabelBytes = 1 << 20

// hostileUTF8Label is a client-controlled value one MiB long with a multi-byte
// rune, so a bound that cuts at a byte offset would leave invalid UTF-8.
func hostileUTF8Label() string { return strings.Repeat("é", hostileLabelBytes/2) }

// Documented label, identifier and retained-header bounds. These literals
// mirror docs/protocol.md and docs/operations.md instead of deriving from the
// production constants: a one-sided production change must fail this contract
// and force the documentation to move with it.
const (
	documentedClientLabelMax       = 256
	documentedClientLanguageMax    = 256
	documentedUserAgentMax         = 1024
	documentedClientIPMax          = 128
	documentedProviderModelMetaMax = 512
	documentedDeclaredSessionMax   = 512
	documentedConversationIDMax    = 514
	documentedHeaderNameMax        = 512
	documentedHeaderValueMax       = 4096
	documentedHeaderValuesMax      = 16
	documentedHeaderMapBytesMax    = 16384
)

// TestDocumentedLabelBoundsMatchConstants pins the documented client-label,
// identifier and retained-header bounds to their single production owners. The
// provider/model record metadata and conversation identifier are checked
// through the real record boundary because metrics owns those package-private
// constants.
func TestDocumentedLabelBoundsMatchConstants(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"client", retainedClientMax, documentedClientLabelMax},
		{"client language", retainedClientLangMax, documentedClientLanguageMax},
		{"user agent", retainedUserAgentMax, documentedUserAgentMax},
		{"client IP", retainedClientIPMax, documentedClientIPMax},
		{"model label", retainedLabelMax, documentedProviderModelMetaMax},
		{"declared session", maxDeclaredSessionBytes, documentedDeclaredSessionMax},
		{"header name", retainedHeaderNameMax, documentedHeaderNameMax},
		{"header value", retainedHeaderValueMax, documentedHeaderValueMax},
		{"header values per name", retainedHeaderValuesMax, documentedHeaderValuesMax},
		{"header map budget", retainedHeadersMaxBytes, documentedHeaderMapBytesMax},
	} {
		if tc.got != tc.want {
			t.Errorf("%s bound = %d, want the documented %d", tc.name, tc.got, tc.want)
		}
	}

	// The declared-session bound plus the s: namespace is the retained
	// conversation identifier the docs describe.
	if got := len(explicitConversationID(strings.Repeat("a", maxDeclaredSessionBytes))); got != documentedConversationIDMax {
		t.Errorf("retained conversation identifier = %d bytes, want the documented %d (512-byte session plus the s: prefix)", got, documentedConversationIDMax)
	}

	hostile := strings.Repeat("m", documentedProviderModelMetaMax+1)
	b := metrics.NewBuffer(1)
	b.Record(&metrics.Record{ID: "metadata-bounds", Provider: hostile, Model: hostile, ProviderModel: hostile})
	rec := b.Snapshot()[0]
	for _, field := range []struct {
		name string
		got  string
	}{
		{"provider", rec.Provider},
		{"model", rec.Model},
		{"provider model", rec.ProviderModel},
	} {
		if len(field.got) != documentedProviderModelMetaMax {
			t.Errorf("%s metadata retained %d bytes, want the documented %d", field.name, len(field.got), documentedProviderModelMetaMax)
		}
	}
}

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
		assertBoundedLabel(t, "client language", clientLang(r), retainedClientLangMax)
	})
	t.Run("language from user agent", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("User-Agent", long+"/1.0")
		assertBoundedLabel(t, "client language", clientLang(r), retainedClientLangMax)
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
	atLang := strings.Repeat("l", retainedClientLangMax)
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
		assertBoundedLabel(t, target.name+" client language", target.rec.ClientLang, retainedClientLangMax)
	}
	if ring.UserAgent != durable[0].UserAgent || ring.Client != durable[0].Client ||
		ring.ClientIP != durable[0].ClientIP || ring.ClientLang != durable[0].ClientLang {
		t.Fatalf("ring and durable labels diverged:\nring=%+v\ndurable=%+v", ring, durable[0])
	}
}

// TestProviderLabelBoundary pins the base-URL authority boundary: a normal
// domain still derives its registrable label, IP authorities keep their
// host:port form, and a header-sized hostname or an implausible port is
// rejected before any record exists.
func TestProviderLabelBoundary(t *testing.T) {
	const dnsHostMax = 253
	cfg := config.Default()
	for _, tc := range []struct {
		base string
		want string
	}{
		{"https://api.fixture.example/v1", "fixture.example"},
		{"http://10.0.0.5:8000/v1", "10.0.0.5:8000"},
		{"http://[::1]:11434/v1", "[::1]:11434"},
		{"http://localhost:11434/v1", "localhost"},
		{"https://api.fixture.example:65535/v1", "fixture.example"},
		{"https://" + strings.Repeat("a", dnsHostMax), strings.Repeat("a", dnsHostMax)},
	} {
		r := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", nil)
		r.Header.Set(hdrBaseURL, tc.base)
		tgt, err := resolveTarget(r, cfg)
		if err != nil {
			t.Fatalf("resolveTarget(%q): %v", tc.base, err)
		}
		if tgt.provider != tc.want {
			t.Fatalf("provider for %q = %q, want %q", tc.base, tgt.provider, tc.want)
		}
	}
	for _, base := range []string{
		"https://" + strings.Repeat("a", dnsHostMax+1),
		"https://api.fixture.example:65536",
		"https://api.fixture.example:999999",
		"https://api.fixture.example:0",
	} {
		r := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", nil)
		r.Header.Set(hdrBaseURL, base)
		if _, err := resolveTarget(r, cfg); err == nil {
			t.Fatalf("resolveTarget(%q) accepted an implausible authority", base)
		}
	}

	// The real request boundary rejects a 900 KiB hostname and never records it.
	buf := metrics.NewBuffer(1)
	p := New(cfg, buf)
	p.client.Store(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("hostile base URL reached the upstream transport")
		return nil, nil
	})})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	r.Header.Set(hdrBaseURL, "http://"+strings.Repeat("a", 900<<10)+"/v1")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("hostile base URL status = %d, want 400", w.Code)
	}
	if buf.Len() != 0 {
		t.Fatalf("hostile base URL produced %d records, want none", buf.Len())
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
