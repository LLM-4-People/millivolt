package proxy

// Generic OpenAI GET /v1/models handling across upstream providers.
//
// The client asks the proxy (an OpenAI-compatible endpoint) for the model list
// ONCE; the proxy constructs the complete answer from the upstream described
// by the per-request X-Proxy-* headers:
//   - cursor:     agent.v1 GetUsableModels (unary Connect-RPC) → OpenAI shape.
//   - anthropic:  Anthropic /v1/models (paginated, different shape) → merged
//                 and translated to the OpenAI shape.
//   - everything else (openai / openai-compatible): the upstream's /v1/models
//                 is fetched, normalized, and (when the provider configures a
//                 models metadata endpoint) enriched before serving.
//
// Construction is additive and honest: upstream fields ride through verbatim;
// the proxy only guarantees the canonical OpenAI wrapper/fields, mirrors the
// two context-window spellings, and merges operator-mapped enrichment fields.
// It never invents model data (no static tables).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/metrics"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
)

// modelsDiscovery is one request-scoped budget, shared across every primary
// page/unary response and optional enrichment. Only discovery allocates it;
// the shared upstream client's stream-safe timeout policy remains unchanged.
type modelsDiscovery struct {
	ctx       context.Context
	bytesLeft int64
	pagesLeft int
}

// serveModels owns the whole upstream discovery deadline, including transparent
// credential refresh. All three formats share the same captured resource limits.
func (s *Server) serveModels(w http.ResponseWriter, r *http.Request, t *target, key string) {
	cfg := s.cfg()
	ctx, cancel := context.WithTimeout(r.Context(), cfg.ModelsDiscoveryTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	d := &modelsDiscovery{ctx: ctx, bytesLeft: cfg.ModelsDiscoveryMaxBytes, pagesLeft: cfg.ModelsDiscoveryMaxPages}
	key = s.resolveKeyTransparent(r, t, key)
	switch t.format {
	case "cursor":
		s.serveCursorModels(d, w, r, t, key)
	case "anthropic":
		s.serveAnthropicModels(d, w, r, t, key)
	default:
		// openai / openai-compatible: fetch, construct, enrich, serve.
		s.serveOpenAIModels(d, w, r, t, key)
	}
}

// modelsUpstreamURL builds the upstream /v1/models URL from the base URL. The
// base may already end in /v1 (clients point at ".../v1"); avoid doubling it.
func modelsUpstreamURL(t *target) string {
	return joinUpstreamURL(t.baseURL, "/v1/models")
}

// serveOpenAIModels fetches an OpenAI-compatible upstream's /v1/models and
// constructs the served list: the canonical {"object":"list","data":[...]}
// wrapper, guaranteed per-entry core fields, mirrored context spellings, and
// the per-provider enrichment merge. An upstream body that is not a
// recognizable list degrades to verbatim passthrough - never a fabricated
// empty list, never a 502 over a merely unfamiliar shape.
func (s *Server) serveOpenAIModels(d *modelsDiscovery, w http.ResponseWriter, r *http.Request, t *target, key string) {
	body, err := s.fetchModelsUpstream(d, t, key, modelsUpstreamURL(t))
	if err != nil {
		writeModelsError(w, err)
		return
	}
	entries, constructed := parseModelsListBody(body)
	if !constructed {
		// Not a shape we construct (foreign envelope or unparsable): relay
		// verbatim - the passthrough contract for anything unknown.
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
		return
	}
	s.emitModelsList(d, w, r, t, key, entries)
}

// emitModelsList is the ONE construction path every format branch ends in:
// per-provider enrichment merge, core-field normalization, and the OpenAI
// list envelope. Provider data is only ever added, never dropped.
func (s *Server) emitModelsList(d *modelsDiscovery, w http.ResponseWriter, r *http.Request, t *target, key string, entries []map[string]any) {
	if err := d.ctx.Err(); err != nil {
		writeModelsError(w, err)
		return
	}
	s.enrichModelEntries(d, t, key, entries)
	entries = normalizeModelEntries(entries, t.provider)
	w.Header().Set("Content-Type", "application/json")
	w.Write(providerformat.ModelsListJSON(entries))
}

// writeModelsError surfaces a models-fetch failure with the uniform 502 JSON
// wrapper.
func writeModelsError(w http.ResponseWriter, err error) {
	http.Error(w, errJSON("api_error", err.Error()), http.StatusBadGateway)
}

// fetchModelsUpstream GETs one upstream models URL with the target's auth
// header and returns the body. Non-200 and transport failures become errors
// (the caller surfaces them); metadata bodies share the bounded discovery
// reader, never the LLM stream path. Provider-configured headers ride along so
// discovery presents the same wire fingerprint as LLM calls.
func (s *Server) fetchModelsUpstream(d *modelsDiscovery, t *target, key, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(d.ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set(t.authHeader, t.authPrefix+key)
	}
	return s.fetchModelsResponse(d, req, t, key)
}

// fetchModelsResponse is the sole response/read/status owner for all discovery
// formats and enrichment. Response.Body is already decoded when net/http
// negotiated gzip; Content-Length is never trusted as the read boundary.
// One extra byte distinguishes an exact fit from truncation. Do not drain a
// rejected body: doing so would bypass both the byte budget and cancellation.
func (s *Server) fetchModelsResponse(d *modelsDiscovery, req *http.Request, t *target, key string) ([]byte, error) {
	if err := d.ctx.Err(); err != nil {
		return nil, err
	}
	if d.pagesLeft <= 0 {
		return nil, fmt.Errorf("model discovery exceeded upstream page limit")
	}
	if d.bytesLeft <= 0 {
		return nil, fmt.Errorf("model discovery exceeded aggregate response byte limit")
	}
	d.pagesLeft--
	// Reuse the existing header policy exactly once. A unary model RPC keeps
	// its protected protocol/auth identity; ordinary GETs (including optional
	// enrichment) let configured headers override the initial auth/version.
	if req.Method == http.MethodPost && t.format == "cursor" {
		s.setCursorIdentity(req, t, key)
		req.Header.Set("Content-Type", "application/proto")
	} else {
		s.applyProviderHeaders(req.Header, t.provider)
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream error: %s", transportErrText(err))
	}
	defer resp.Body.Close()
	remaining := d.bytesLeft
	body, err := io.ReadAll(io.LimitReader(resp.Body, remaining+1))
	d.bytesLeft -= int64(len(body))
	if int64(len(body)) > remaining {
		return nil, fmt.Errorf("model discovery exceeded aggregate response byte limit")
	}
	if err != nil {
		return nil, fmt.Errorf("read upstream: %w", err)
	}
	if err := d.ctx.Err(); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream models: HTTP %d: %s", resp.StatusCode, truncateForErr(body))
	}
	return body, nil
}

// parseModelsListBody recognizes the upstream list shapes observed across the
// OpenAI-compatible ecosystem: {"data":[...]} (OpenAI, Groq, xAI, vLLM), a
// bare top-level array (Together), or a body already carrying entries under a
// foreign key (served verbatim by the caller). ok=false means "not ours to
// construct".
func parseModelsListBody(body []byte) (entries []map[string]any, ok bool) {
	var wrapper struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Data != nil {
		return wrapper.Data, true
	}
	if err := json.Unmarshal(body, &entries); err == nil && entries != nil {
		return entries, true
	}
	return nil, false
}

// normalizeModelEntries guarantees the canonical OpenAI list invariants on
// every served entry, additively: only objects with a non-empty id survive
// (an id-less model is unusable for every client and would break strict SDKs),
// missing core fields are backfilled (object:"model", created 0 = unknown,
// owned_by = the provider label), and the two context-window spellings are
// mirrored so either-reading client sees the value (ecosystems split:
// OpenRouter/xAI/Together emit context_length, Groq emits context_window).
// Existing upstream fields always win - a mirror never overwrites.
func normalizeModelEntries(entries []map[string]any, provider string) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if e == nil {
			continue
		}
		if id, _ := e["id"].(string); strings.TrimSpace(id) == "" {
			continue
		}
		if _, ok := e["object"]; !ok {
			e["object"] = "model"
		}
		if _, ok := e["created"]; !ok {
			e["created"] = 0
		}
		if _, ok := e["owned_by"]; !ok {
			e["owned_by"] = provider
		}
		mirrorContextWindow(e)
		out = append(out, e)
	}
	return out
}

// mirrorContextWindow duplicates whichever context spelling is present into
// the other, when the other is absent. Both must stay one semantic fact with
// two ecosystem spellings - never two independent values.
func mirrorContextWindow(e map[string]any) {
	cl, hasCL := jsonNumber(e["context_length"])
	cw, hasCW := jsonNumber(e["context_window"])
	if hasCL && !hasCW {
		e["context_window"] = cl
	} else if hasCW && !hasCL {
		e["context_length"] = cw
	}
}

// jsonNumber reports v as a JSON number (encoding/json decodes numbers into
// float64). Nulls and non-numbers are "absent" - a null context_length must
// not invent a context_window.
func jsonNumber(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// enrichModelEntries merges per-provider model metadata (config
// providers.<label>.models_path + models_keys) into the entries. The
// enrichment endpoint is fetched with the same auth as the main list; its
// entries join by "id"; a mapped output field only fills entries that do not
// already carry it (the upstream's own value always wins). Any failure -
// fetch, status, shape - degrades to the unenriched list: enrichment is a
// completeness win, never a hard dependency.
func (s *Server) enrichModelEntries(d *modelsDiscovery, t *target, key string, entries []map[string]any) {
	ov, ok := s.cfg().Providers[t.provider]
	if !ok || ov.ModelsPath == "" || len(ov.ModelsKeys) == 0 || len(entries) == 0 {
		return
	}
	body, err := s.fetchModelsUpstream(d, t, key, joinUpstreamURL(t.baseURL, ov.ModelsPath))
	if err != nil {
		return
	}
	src, ok := parseEnrichmentEntries(body)
	if !ok {
		return
	}
	byID := make(map[string]map[string]any, len(src))
	for _, e := range src {
		if id, _ := e["id"].(string); id != "" {
			byID[id] = e
		}
	}
	for _, e := range entries {
		id, _ := e["id"].(string)
		enr, ok := byID[id]
		if !ok {
			continue
		}
		for field, path := range ov.ModelsKeys {
			if _, exists := e[field]; exists {
				continue
			}
			if v, found := metrics.DigJSON(enr, path); found {
				e[field] = v
			}
		}
	}
}

// parseEnrichmentEntries - the enrichment endpoint may nest its entry array
// under "data" (OpenAI style), "models" (xAI's language-models), or return a
// bare root array; anything else is "no enrichment available".
func parseEnrichmentEntries(body []byte) ([]map[string]any, bool) {
	var wrapper struct {
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil {
		if wrapper.Data != nil {
			return wrapper.Data, true
		}
		if wrapper.Models != nil {
			return wrapper.Models, true
		}
	}
	var arr []map[string]any
	if err := json.Unmarshal(body, &arr); err == nil && arr != nil {
		return arr, true
	}
	return nil, false
}

// serveAnthropicModels pages Anthropic's /v1/models (collecting all pages) and
// returns the merged list in the OpenAI shape. Anthropic requires x-api-key and
// an anthropic-version header; both are set here (the client sends an
// OpenAI-style Authorization, which Anthropic would not accept).
func (s *Server) serveAnthropicModels(d *modelsDiscovery, w http.ResponseWriter, r *http.Request, t *target, key string) {
	const anthropicVersion = "2023-06-01" // required on every Anthropic API request
	const pageLimit = "1000"              // Anthropic caps limit at 1000

	var entries []map[string]any
	seenCursors := make(map[string]struct{})
	after := ""
	for {
		u := modelsUpstreamURL(t) + "?limit=" + pageLimit
		if after != "" {
			u += "&after_id=" + url.QueryEscape(after)
		}
		req, err := http.NewRequestWithContext(d.ctx, http.MethodGet, u, nil)
		if err != nil {
			writeModelsError(w, err)
			return
		}
		// Anthropic auth: x-api-key (not Bearer). The resolved key may carry an
		// authHeader/authPrefix override; honor it, defaulting to x-api-key.
		if key != "" {
			if t.authHeader != "" {
				req.Header.Set(t.authHeader, t.authPrefix+key)
			} else {
				req.Header.Set("x-api-key", key)
			}
		}
		req.Header.Set("anthropic-version", anthropicVersion)

		body, err := s.fetchModelsResponse(d, req, t, key)
		if err != nil {
			writeModelsError(w, err)
			return
		}
		page, err := providerformat.TranslateAnthropicModels(body)
		if err != nil {
			writeModelsError(w, fmt.Errorf("decode anthropic models: %w", err))
			return
		}
		entries = append(entries, page.Entries...)
		if page.NextAfter == "" {
			break
		}
		if _, repeated := seenCursors[page.NextAfter]; repeated {
			writeModelsError(w, fmt.Errorf("model discovery repeated pagination cursor"))
			return
		}
		seenCursors[page.NextAfter] = struct{}{}
		after = page.NextAfter
	}

	s.emitModelsList(d, w, r, t, key, entries)
}
