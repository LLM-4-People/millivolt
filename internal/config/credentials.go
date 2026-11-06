package config

import (
	"strings"
)

// This file is one vocabulary with three consumers, deliberately in the leaf
// package all three can reach:
//
//   - the request-overrides load gate (ForbiddenOverrideHeader),
//   - the send-stage belt (overrideHeaderAuthOwned in internal/proxy),
//   - capture and debug retention (redactHeaderName in internal/proxy),
//   - saved-config redaction for the Settings and operator CLI surfaces
//     (RedactProviderHeaders below).
//
// An override header value is operator-supplied and is stamped onto a request
// whose destination the caller chooses with X-Proxy-Base-URL, which is
// unrestricted by default. If the value is a secret, the caller collects it. A
// name allowlist of vendor spellings cannot be complete - X-Auth-Key, the Azure
// X-Functions-Key and AWS X-Amz-Credential are all credentials whose names spell
// no common marker - so the classification is deny-by-default over role words
// instead, and the carve-outs below are the only names admitted.

// credentialRoleMarkers are the word-level spellings that mean "this header
// carries, addresses or proves a credential". Any occurrence anywhere in the
// name is enough; a false positive costs a refused override rule, while a false
// negative costs a leaked operator secret.
//
// "auth" is the broadest and deliberately catches authorization, x-auth,
// x-auth-key, x-authentication and x-auth-token. It also catches the rare
// non-secret x-authorized-scopes; over-denying that is the intended direction.
// "signature" covers signing keys and HMAC headers, which verify or replace a
// credential even when the value is derived.
var credentialRoleMarkers = []string{
	"authorization", "authorisation",
	"auth",
	"api-key", "apikey", "api_key",
	"secret",
	"password", "passwd", "passphrase", "pwd",
	"credential",
	"bearer",
	"jwt",
	"signature",
	"cookie",
	"key",
	"token",
}

// nonSecretKeyWords mark a name that mentions a key without carrying key
// material: an identifier, a cache slot, or an idempotency token.
var nonSecretKeyWords = []string{"key-id", "keyid", "key_id", "key-name", "keyname", "key_name", "idempotency", "cache"}

// nonSecretTokenWords mark a measurement rather than a credential: the
// token-accounting counters and identifiers of the rate-limit families, plus
// size and budget names. Plural "tokens" is the common accounting spelling
// (x-prompt-tokens, anthropic-ratelimit-input-tokens-remaining).
var nonSecretTokenWords = []string{
	"count", "total", "used", "usage", "budget", "remaining", "reset",
	"limit", "size", "length", "estimate", "rate", "window", "quota",
	"capacity", "max", "tokens", "tokenizer",
}

// nonSecretExactHeaderNames are names that match a credential marker but carry
// no secret, so an operator may still set them. Keep this list short and
// justified one entry at a time.
var nonSecretExactHeaderNames = map[string]struct{}{
	"x-authorized-scopes": {}, // scope names, not a credential
	"key":                 {}, // a bare header called "key" is a lookup key, not a secret; see below
}

// credentialExactHeaderNames are the control-channel and cookie names whose
// spelling carries no role marker but whose value is a secret or a credential
// channel. The x-proxy- names are the proxy's own request channel: the header
// map and the forwarded credentials are refused as override actions and
// redacted from retention.
var credentialExactHeaderNames = map[string]struct{}{
	"cookie":                {},
	"set-cookie":            {},
	"x-proxy-refresh-token": {},
	"x-proxy-access-token":  {},
	"x-proxy-key":           {},
	"x-proxy-headers":       {},
}

// CredentialHeaderName reports whether an HTTP header name is credential-class,
// meaning its value is expected to carry a secret.
func CredentialHeaderName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	if _, ok := credentialExactHeaderNames[n]; ok {
		return true
	}
	if _, ok := nonSecretExactHeaderNames[n]; ok {
		return false
	}
	// A name that mentions a key or a token is judged by its qualifier first:
	// "x-api-key-id" names an identifier and "x-prompt-tokens" counts tokens,
	// while "x-session-key" and "x-auth-token" carry the material itself.
	hasKey := strings.Contains(n, "key")
	hasToken := strings.Contains(n, "token")
	if hasKey && containsAny(n, nonSecretKeyWords) && !containsAny(n, strongCredentialMarkers) {
		hasKey = false
	}
	if hasToken && containsAny(n, nonSecretTokenWords) && !containsAny(n, strongCredentialMarkers) {
		hasToken = false
	}
	if hasKey || hasToken {
		return true
	}
	return containsAny(n, strongCredentialMarkers)
}

// strongCredentialMarkers are the role markers other than bare key/token. They
// override the accounting carve-outs, so "x-auth-tokens" stays credential even
// though it looks like a counter.
var strongCredentialMarkers = []string{
	"authorization", "authorisation", "auth", "api-key", "apikey", "api_key",
	"secret", "password", "passwd", "passphrase", "pwd", "credential",
	"bearer", "jwt", "signature", "cookie",
}

func containsAny(s string, words []string) bool {
	for _, word := range words {
		if strings.Contains(s, word) {
			return true
		}
	}
	return false
}

// RedactProviderHeaders returns a shallow copy of a config values map with
// every credential-class provider header value replaced by marker. This is the
// one owner of where a saved config document carries a secret: provider
// overrides' headers. Token counters, windows and rate-limit values are
// measurements and stay intact through CredentialHeaderName.
//
// The copy shares every non-provider value with the input; callers treat the
// result as read-only, exactly like Config.Map.
func RedactProviderHeaders(values map[string]any, marker string) map[string]any {
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = value
	}
	// A JSON-decoded document carries map[string]any; Config.Map carries the
	// typed map[string]ProviderOverride. Both are legitimate saved-config
	// shapes, and a redactor that silently skipped the typed one would fail
	// open on the in-process path.
	switch providers := values["providers"].(type) {
	case map[string]any:
		redacted := make(map[string]any, len(providers))
		for name, provider := range providers {
			switch fields := provider.(type) {
			case map[string]any:
				redacted[name] = redactProviderFields(fields, marker)
			case ProviderOverride:
				redacted[name] = redactProviderOverride(fields, marker)
			default:
				redacted[name] = provider
			}
		}
		out["providers"] = redacted
	case map[string]ProviderOverride:
		redacted := make(map[string]ProviderOverride, len(providers))
		for name, provider := range providers {
			redacted[name] = redactProviderOverride(provider, marker)
		}
		out["providers"] = redacted
	}
	return out
}

// RedactLastReload returns a shallow copy of a config document's last_reload
// value with its error text replaced by marker. The outcome metadata stays so
// a caller still learns that a reload failed; the quoted text is omitted
// because the loader can echo a rejected value, credentials included. Shapes
// other than an object pass through unchanged.
func RedactLastReload(reload any, marker string) any {
	fields, ok := reload.(map[string]any)
	if !ok {
		return reload
	}
	out := make(map[string]any, len(fields))
	for key, value := range fields {
		out[key] = value
	}
	if message, ok := out["error"].(string); ok && strings.TrimSpace(message) != "" {
		out["error"] = marker
	}
	return out
}

func redactProviderFields(fields map[string]any, marker string) map[string]any {
	clone := make(map[string]any, len(fields))
	for key, value := range fields {
		clone[key] = value
	}
	switch headers := fields["headers"].(type) {
	case map[string]any:
		redacted := make(map[string]any, len(headers))
		for header, value := range headers {
			if CredentialHeaderName(header) {
				redacted[header] = marker
			} else {
				redacted[header] = value
			}
		}
		clone["headers"] = redacted
	case map[string]string:
		redacted := make(map[string]string, len(headers))
		for header, value := range headers {
			if CredentialHeaderName(header) {
				redacted[header] = marker
			} else {
				redacted[header] = value
			}
		}
		clone["headers"] = redacted
	}
	return clone
}

func redactProviderOverride(provider ProviderOverride, marker string) ProviderOverride {
	clone := provider
	if provider.Headers == nil {
		return clone
	}
	clone.Headers = make(map[string]string, len(provider.Headers))
	for header, value := range provider.Headers {
		if CredentialHeaderName(header) {
			clone.Headers[header] = marker
		} else {
			clone.Headers[header] = value
		}
	}
	return clone
}
