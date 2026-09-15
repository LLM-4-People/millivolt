package proxy

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestRetryableUpstreamErrorClass pins the one precedence chain that decides
// whether an in-band provider error authorizes a transparent re-send: quota
// deny > operator extension > client-fault deny (against the built-ins) >
// built-in retryable vocabulary > fail closed. Every rule the streaming and
// non-streaming rescue sites rely on has a row here, so a vocabulary or
// precedence regression reddens this table before it can reach the relay.
func TestRetryableUpstreamErrorClass(t *testing.T) {
	newServer := func(ext ...string) *Server {
		cfg := config.Default()
		cfg.RetryableErrorClasses = ext
		return New(cfg, metrics.Noop{})
	}
	cases := []struct {
		name string
		ext  []string
		typ  string
		code string
		want bool
		why  string
	}{
		{name: "coralbricks incident shape (api_error/internal_error)", typ: "api_error", code: "internal_error", want: true, why: "built-in: Anthropic 500 type and the gateway code spelling"},
		{name: "openai 500 type", typ: "server_error", want: true, why: "built-in"},
		{name: "openai 503 current type", typ: "service_unavailable_error", want: true, why: "built-in"},
		{name: "openai 503 code", typ: "service_unavailable_error", code: "server_is_overloaded", want: true, why: "built-in via code"},
		{name: "anthropic 529", typ: "overloaded_error", want: true, why: "built-in"},
		{name: "anthropic timeout", typ: "timeout_error", want: true, why: "built-in"},
		{name: "case-insensitive type", typ: "API_Error", want: true, why: "the envelope vocabulary lowercases"},
		{name: "case-insensitive code", typ: "weird_gateway", code: "Internal_Server_Error", want: true, why: "the gateway code spelling lowercases"},
		{name: "unknown class", typ: "model_warming_up", want: false, why: "fail closed: verbatim relay"},
		{name: "in-band rate limit is flow control", typ: "rate_limit_error", want: false, why: "no in-band Retry-After exists; the scheduler owns pacing"},
		{name: "client-fault type", typ: "invalid_request_error", want: false, why: "the request itself is wrong; a re-send repeats it"},
		{name: "client-fault type with a server-shaped code", typ: "invalid_request_error", code: "internal_error", want: false, why: "the type is the authoritative class field"},
		{name: "permission type", typ: "permission_error", want: false, why: "client fault"},
		{name: "quota type", typ: "insufficient_quota", code: "internal_error", want: false, why: "quota deny outranks every server-shaped code"},
		{name: "quota code", typ: "weird_gateway", code: "exceeded_current_quota_error", want: false, why: "quota deny outranks the unknown type"},
		{name: "operator extension authorizes a custom spelling", ext: []string{"model_warming_up"}, typ: "model_warming_up", want: true, why: "the extension list is the operator's explicit vocabulary"},
		{name: "operator extension matches the code field", ext: []string{"gateway_hiccup"}, typ: "weird_gateway", code: "gateway_hiccup", want: true, why: "extensions match type or code like the built-ins"},
		{name: "operator extension overrides the client-fault deny", ext: []string{"invalid_request_error"}, typ: "invalid_request_error", want: true, why: "a gateway that mislabels a transient failure can be opted back in"},
		{name: "operator extension never overrides the quota deny", ext: []string{"insufficient_quota"}, typ: "insufficient_quota", want: false, why: "waiting cannot restore credits"},
		{name: "extension is case-insensitive", ext: []string{"Model_Warming_Up"}, typ: "model_warming_up", want: true, why: "validation and matching agree on case"},
		{name: "extension does not leak into other classes", ext: []string{"model_warming_up"}, typ: "unrelated_class", want: false, why: "additive only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(tc.ext...)
			if got := s.retryableUpstreamErrorClass(tc.typ, tc.code); got != tc.want {
				t.Errorf("retryableUpstreamErrorClass(%q, %q) = %v, want %v (%s)", tc.typ, tc.code, got, tc.want, tc.why)
			}
		})
	}
}
