package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/plugincred"
)

// Generic http:fetch bounds. These guardrails are host-owned: a guest cannot
// widen them by changing its request payload.
const (
	maxFetchURLBytes      = 2048
	maxFetchHeaderBytes   = 4096
	maxFetchHeaderEntries = 16
	maxFetchCredential    = 8 << 10
	maxFetchResponseBytes = 64 << 10
	maxFetchRequestBytes  = maxFetchURLBytes + maxFetchHeaderBytes + 4096
	maxFetchTimeout       = 5 * time.Second
	requestCredentialID   = "request"
)

var (
	fetchEnvPlaceholder = regexp.MustCompile(`\$\{env:([A-Z][A-Z0-9_]{0,63})\}`)
	fetchCredentialID   = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
)

type httpFetchRequest struct {
	URL              string            `json:"url"`
	Method           string            `json:"method,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	CredentialID     string            `json:"credential_id,omitempty"`
	CredentialHeader string            `json:"credential_header,omitempty"`
	TimeoutMs        int               `json:"timeout_ms,omitempty"`
	MaxResponseBytes int               `json:"max_response_bytes,omitempty"`
}

type httpFetchResponse struct {
	Status int    `json:"status"`
	Body   string `json:"body,omitempty"`
	Error  string `json:"error,omitempty"`
}

// pluginHTTPClient never follows redirects: the allowlist is checked once on
// the expanded URL, so following a redirect would bypass that decision.
var pluginHTTPClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func normalizeHTTPHosts(hosts []string) map[string]struct{} {
	out := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" {
			out[host] = struct{}{}
		}
	}
	return out
}

func credentialEnvName(id string) (string, bool) {
	if !fetchCredentialID.MatchString(id) {
		return "", false
	}
	name := strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(id))
	return "MILLIVOLT_PLUGIN_HTTP_CREDENTIAL_" + name, true
}

func expandFetchURL(raw string, lookup func(string) (string, bool)) (string, error) {
	missing := false
	expanded := fetchEnvPlaceholder.ReplaceAllStringFunc(raw, func(match string) string {
		name := match[len("${env:") : len(match)-1]
		value, ok := lookup(name)
		if !ok || value == "" || strings.ContainsAny(value, " \t\r\n/") {
			missing = true
			return ""
		}
		return value
	})
	if missing {
		return "", fmt.Errorf("fetch URL environment is unavailable")
	}
	if len(expanded) > maxFetchURLBytes {
		return "", fmt.Errorf("fetch URL exceeds host bounds")
	}
	return expanded, nil
}

// fetchHTTP implements the generic outbound fetch capability. It is used by
// the WASM host import and is independently testable with a local server.
func fetchHTTP(ctx context.Context, raw []byte, allowed map[string]struct{}) httpFetchResponse {
	return fetchHTTPWithEnv(ctx, raw, allowed, os.LookupEnv, plugincred.From(ctx))
}

func fetchHTTPWithEnv(ctx context.Context, raw []byte, allowed map[string]struct{}, lookup func(string) (string, bool), cred *plugincred.Credential) httpFetchResponse {
	if len(raw) == 0 || len(raw) > maxFetchRequestBytes {
		return httpFetchResponse{Error: "fetch request exceeds host bounds"}
	}
	var req httpFetchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return httpFetchResponse{Error: "invalid fetch request"}
	}
	if len(req.URL) == 0 || len(req.URL) > maxFetchURLBytes {
		return httpFetchResponse{Error: "invalid fetch URL"}
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet {
		return httpFetchResponse{Error: "fetch method must be GET"}
	}
	expandedURL, err := expandFetchURL(req.URL, lookup)
	if err != nil {
		return httpFetchResponse{Error: err.Error()}
	}
	u, err := url.Parse(expandedURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return httpFetchResponse{Error: "invalid fetch URL"}
	}
	if _, ok := allowed[strings.ToLower(u.Host)]; !ok {
		return httpFetchResponse{Error: "fetch host is not allowlisted"}
	}

	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 || timeout > maxFetchTimeout {
		timeout = maxFetchTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := http.NewRequestWithContext(callCtx, method, u.String(), nil)
	if err != nil {
		return httpFetchResponse{Error: "invalid fetch request"}
	}
	headerBytes := 0
	for name, value := range req.Headers {
		if name == "" || strings.ContainsAny(name, ":\r\n") || strings.ContainsAny(value, "\r\n") {
			return httpFetchResponse{Error: "invalid fetch header"}
		}
		headerBytes += len(name) + len(value)
		if headerBytes > maxFetchHeaderBytes || len(req.Headers) > maxFetchHeaderEntries {
			return httpFetchResponse{Error: "fetch headers exceed host bounds"}
		}
		out.Header.Set(name, value)
	}
	if req.CredentialID == requestCredentialID {
		if cred == nil || cred.Header == "" || cred.Value == "" ||
			len(cred.Value) > maxFetchCredential || strings.ContainsAny(cred.Value, "\r\n") {
			return httpFetchResponse{Error: "request credential unavailable"}
		}
		if req.CredentialHeader != "" && !strings.EqualFold(req.CredentialHeader, cred.Header) {
			return httpFetchResponse{Error: "fetch credential header mismatch"}
		}
		headerBytes += len(cred.Header) + len(cred.Value)
		if headerBytes > maxFetchHeaderBytes {
			return httpFetchResponse{Error: "fetch headers exceed host bounds"}
		}
		out.Header.Set(cred.Header, cred.Value)
	} else if req.CredentialID != "" {
		if req.CredentialHeader == "" || strings.ContainsAny(req.CredentialHeader, ":\r\n") {
			return httpFetchResponse{Error: "invalid fetch credential header"}
		}
		envName, ok := credentialEnvName(req.CredentialID)
		if !ok {
			return httpFetchResponse{Error: "invalid fetch credential id"}
		}
		value, ok := lookup(envName)
		if !ok || value == "" || len(value) > maxFetchCredential || strings.ContainsAny(value, "\r\n") {
			return httpFetchResponse{Error: "fetch credential unavailable"}
		}
		headerBytes += len(req.CredentialHeader) + len(value)
		if headerBytes > maxFetchHeaderBytes {
			return httpFetchResponse{Error: "fetch headers exceed host bounds"}
		}
		out.Header.Set(req.CredentialHeader, value)
	}

	resp, err := pluginHTTPClient.Do(out)
	if err != nil {
		return httpFetchResponse{Error: fmt.Sprintf("fetch failed: %v", err)}
	}
	defer resp.Body.Close()

	capBytes := req.MaxResponseBytes
	if capBytes <= 0 || capBytes > maxFetchResponseBytes {
		capBytes = maxFetchResponseBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(capBytes)+1))
	if err != nil {
		return httpFetchResponse{Status: resp.StatusCode, Error: "fetch body read failed"}
	}
	if len(body) > capBytes {
		return httpFetchResponse{Status: resp.StatusCode, Error: "fetch body exceeds host cap"}
	}
	return httpFetchResponse{Status: resp.StatusCode, Body: string(body)}
}
