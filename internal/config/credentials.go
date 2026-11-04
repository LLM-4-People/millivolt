package config

import "strings"

// CredentialHeaderName reports whether an HTTP header name is credential-class:
// its value is expected to carry a secret rather than ordinary metadata.
//
// This is one vocabulary with two consumers, deliberately kept in the leaf
// package both can reach. The request-overrides grammar denies these names at
// load, because an override value is operator-supplied and is stamped onto a
// request whose destination the caller chooses: an operator secret placed in a
// non-denied header name would be delivered to that caller's host. The capture
// and debug retention boundary redacts the same names, so a stored capture does
// not keep a credential the wire carried.
//
// The match is deny-by-default over substrings rather than an allowlist of
// vendor spellings: "authorization", "api-key", "secret" and "token" cover the
// shapes that actually appear, including spellings this tree has never seen.
// The accounting families are the explicit exemptions - rate-limit counters are
// numbers, not credentials - and the client queue-limit control header is named
// for its budget, not for a key.
func CredentialHeaderName(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "authorization", "proxy-authorization", "x-api-key", "api-key",
		"cookie", "set-cookie", "x-proxy-refresh-token", "x-proxy-access-token",
		"x-proxy-key", "x-proxy-headers":
		return true
	}
	if strings.Contains(n, "authorization") || strings.Contains(n, "api-key") {
		return true
	}
	if strings.Contains(n, "secret") {
		return true
	}
	// "token" deny-by-default with explicit accounting exemptions: the
	// OpenAI x-ratelimit-* and Anthropic anthropic-ratelimit-* families
	// carry token-budget counters, not credentials, and x-proxy-limit-tokens
	// names a queue budget. Every other token-bearing name stays credential.
	// The ratelimit exemptions are prefix-wide, so a token-named header under
	// them (e.g. x-ratelimit-token) is exempt too; accepted residual, since
	// any header name can carry data and the override grammar still refuses
	// the control prefix.
	if strings.Contains(n, "token") &&
		!strings.HasPrefix(n, "x-ratelimit") &&
		!strings.HasPrefix(n, "anthropic-ratelimit") &&
		n != "x-proxy-limit-tokens" {
		return true
	}
	return strings.Contains(n, "key") && strings.Contains(n, "api")
}
