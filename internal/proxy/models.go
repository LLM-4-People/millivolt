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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/metrics"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
)

const (
	// modelsDiscoveryMaxEntries bounds the model entries one discovery request
	// may retain across every primary page and enrichment source. A compact
	// 8 MiB body of {"id":"x"} objects holds over 600k entries, and one
	// decoded map per entry costs hundreds of MiB; real provider catalogs are
	// far smaller. The raw-byte budget alone therefore cannot bound discovery
	// memory. Internal safety guardrail, not user-tunable; exceeding it fails
	// closed, never a partial list.
	modelsDiscoveryMaxEntries = 10_000
	// modelsDiscoveryOutputEntryAllowance is the per-entry headroom added to
	// the configured discovery byte cap when deriving the rendered-list bound.
	// Normalization adds canonical fields that upstream bytes do not carry
	// (object, created, owned_by, and a mirrored context number), and the
	// provider label can be longer than a compact entry. 512 bytes per entry
	// is far above that fixed expansion and covers the envelope separators.
	// Internal safety guardrail, not user-tunable.
	modelsDiscoveryOutputEntryAllowance = 512
)

// modelsDiscoveryOutputLimit derives the rendered-list byte bound from the
// configured aggregate discovery byte cap. Valid UTF-8 JSON strings re-encode
// to no more bytes than their escaped upstream spelling, so the configured cap
// plus the entry-normalization allowance is the authoritative derived bound;
// malformed UTF-8 can expand during re-encoding and still fails closed here.
// The limit is captured with the rest of the discovery budget.
func modelsDiscoveryOutputLimit(maxBytes int64) int {
	return int(maxBytes) + modelsDiscoveryMaxEntries*modelsDiscoveryOutputEntryAllowance
}

// errModelsDiscoveryEntries is the stable fail-closed reason for a discovery
// source carrying more entries than modelsDiscoveryMaxEntries.
var errModelsDiscoveryEntries = errors.New("model discovery entry limit exceeded")

// modelsDiscovery is one request-scoped budget, shared across every primary
// page/unary response and optional enrichment. Only discovery allocates it;
// the shared upstream client's stream-safe timeout policy remains unchanged.
type modelsDiscovery struct {
	ctx       context.Context
	bytesLeft int64
	pagesLeft int
	// entriesLeft is the discovery-wide entry budget. Every producer debits it
	// through consumeEntries before emitting the constructed list.
	entriesLeft int
	// outputLimit is the rendered-list byte bound derived from the configured
	// discovery byte cap (modelsDiscoveryOutputLimit). It is captured once so
	// a hot reload cannot raise a request that already read its inputs.
	outputLimit int
	// releaseResponse holds the process-wide reservation for the page
	// currently being decoded. Pagination releases it before the next fetch,
	// so consecutive pages never accumulate reservations.
	releaseResponse func()
}

// consumeEntries debits the discovery-wide entry budget and fails closed when
// a source would push the retained list past modelsDiscoveryMaxEntries.
func (d *modelsDiscovery) consumeEntries(n int) error {
	if n > d.entriesLeft {
		return errModelsDiscoveryEntries
	}
	d.entriesLeft -= n
	return nil
}

func (d *modelsDiscovery) releaseResponseBuffer() {
	if d.releaseResponse != nil {
		d.releaseResponse()
		d.releaseResponse = nil
	}
}

// serveModels owns the whole upstream discovery deadline, including transparent
// credential refresh. All three formats share the same captured resource limits.
func (s *Server) serveModels(w http.ResponseWriter, r *http.Request, t *target, key string) {
	cfg := s.cfg()
	ctx, cancel := context.WithTimeout(r.Context(), cfg.ModelsDiscoveryTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	d := &modelsDiscovery{ctx: ctx, bytesLeft: int64(cfg.ModelsDiscoveryMaxBytes), pagesLeft: cfg.ModelsDiscoveryMaxPages, entriesLeft: modelsDiscoveryMaxEntries, outputLimit: modelsDiscoveryOutputLimit(int64(cfg.ModelsDiscoveryMaxBytes))}
	defer d.releaseResponseBuffer()
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
	entries, constructed, err := unwrapEntries(body, d.entriesLeft, "data")
	if err != nil {
		writeModelsError(w, err)
		return
	}
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
	if err := d.consumeEntries(len(entries)); err != nil {
		writeModelsError(w, err)
		return
	}
	s.enrichModelEntries(d, t, key, entries)
	entries = normalizeModelEntries(entries, t.provider)
	out, err := providerformat.ModelsListJSON(entries, d.outputLimit)
	if err != nil {
		writeModelsError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

// writeModelsError surfaces a models-discovery failure - request build, fetch,
// or decode - with the uniform 502 JSON wrapper. Response-buffer admission is
// the one retryable exception: it happens before the status line is committed,
// so it carries the same 503 + Retry-After as the translated relay.
func writeModelsError(w http.ResponseWriter, err error) {
	if errors.Is(err, errResponseBufferBudget) {
		writeResponseBufferBudget(w, nil, ir.Chat)
		return
	}
	writeProtocolHTTPError(w, ir.Chat, http.StatusBadGateway, typeAPIError, err.Error())
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
	setResolvedAuth(req.Header, t, key)
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
	// The previous page has been decoded into entries (or written verbatim)
	// before discovery asks for the next one, so release its reservation now.
	d.releaseResponseBuffer()
	d.pagesLeft--
	// Reuse the existing header policy exactly once. A unary model RPC keeps
	// its protected protocol/auth identity; ordinary GETs (including optional
	// enrichment) let configured headers override the initial auth/version.
	if req.Method == http.MethodPost && t.format == "cursor" {
		s.setCursorIdentity(req, t, key)
		req.Header.Set("Content-Type", "application/proto")
	} else {
		s.applyProviderHeaders(req.Header, t.provider)
		stripTransportModeHeaders(req.Header)
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream error: %s", transportErrText(err))
	}
	defer resp.Body.Close()
	remaining := d.bytesLeft
	// Reserve the whole-document buffer process-wide before reading. The
	// reservation covers the current body until the caller has decoded or
	// written it; serveModels releases the last one on return.
	reserve := responseReadReserve(resp.ContentLength, remaining+1)
	if !s.respBudget.acquire(reserve) {
		return nil, errResponseBufferBudget
	}
	d.releaseResponse = func() { s.respBudget.release(reserve) }
	body, err := s.readWholeDocument(d.ctx, resp.Body, remaining+1)
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

// unwrapEntries returns a provider models/metadata body's entry array,
// recognizing the shapes observed across the OpenAI-compatible ecosystem:
// the first present key of keys ({"data":[...]} - OpenAI, Groq, xAI, vLLM;
// {"data","models"} - xAI's language-models endpoint) or a bare top-level
// array (Together). ok=false means "not ours to construct" (a foreign
// envelope or unparsable body the caller relays verbatim / skips).
//
// limit bounds the entries decoded into maps before the decode: a compact
// array of hundreds of thousands of tiny entries would otherwise expand into
// hundreds of MiB. A recognized array past the limit returns
// errModelsDiscoveryEntries (never a partial list); a merely unfamiliar shape
// keeps the verbatim contract.
//
// A requested key whose value is not an entry array rejects the wrapper as a
// whole - matching a strict struct decode, where one bad field fails the
// document - while unrequested keys stay ignored.
func unwrapEntries(body []byte, limit int, keys ...string) ([]map[string]any, bool, error) {
	raw, ok := selectModelArray(body, keys...)
	if !ok {
		return nil, false, nil
	}
	if err := checkModelArrayCount(raw, limit); err != nil {
		if errors.Is(err, errModelsDiscoveryEntries) {
			return nil, false, err
		}
		return nil, false, nil
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, false, nil
	}
	return entries, true, nil
}

// selectModelArray locates the first recognized entry array without decoding
// its elements or its sibling members. json.Valid owns whole-document validity
// (the same syntax gate as the map decode it replaces: trailing garbage,
// escapes, numeric spelling and nesting depth all fail exactly as before),
// and the lexical walk records only the requested keys' value spans. An
// envelope with hundreds of thousands of unrelated members therefore costs no
// map entry, key header or raw-message slice per member; memory stays at the
// caller's bounded body plus this small fixed selection table.
//
// The envelope rules match the previous direct decode: a present requested
// key must hold an array (null is "absent"), every requested key is validated
// before one is selected, duplicate members keep encoding/json's last-wins
// semantics, nested members are never candidates, and only when no requested
// key holds an array does the bare top-level array shape apply.
func selectModelArray(body []byte, keys ...string) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, false
	}
	if trimmed[0] != '{' {
		if trimmed[0] != '[' {
			return nil, false
		}
		return body, true
	}
	// A malformed top-level object is not a recognized envelope (the caller
	// relays it verbatim), never a partially understood list. Validity first
	// lets the lexical pass below trust every member span it skips.
	if !json.Valid(body) {
		return nil, false
	}
	spans := make([]modelMemberSpan, len(keys))
	i := metrics.SkipSpace(body, 0)
	i++ // '{', guaranteed by the trimmed root above
	for {
		i = metrics.SkipSpace(body, i)
		if i >= len(body) {
			return nil, false
		}
		if body[i] == '}' {
			break
		}
		keyStart := i
		keyEnd := metrics.SkipJSONString(body, i)
		if keyEnd < 0 {
			return nil, false
		}
		key := body[keyStart:keyEnd]
		i = metrics.SkipSpace(body, keyEnd)
		if i >= len(body) || body[i] != ':' {
			return nil, false
		}
		i = metrics.SkipSpace(body, i+1)
		valueStart := i
		valueEnd := metrics.SkipJSONValue(body, i)
		if valueEnd <= valueStart {
			return nil, false
		}
		for idx := range keys {
			if modelJSONKeyEquals(key, keys[idx]) {
				// Later occurrences overwrite earlier ones: encoding/json's
				// object decode retains the last duplicate value.
				spans[idx] = modelMemberSpan{start: valueStart, end: valueEnd, present: true}
			}
		}
		i = metrics.SkipSpace(body, valueEnd)
		if i < len(body) && body[i] == ',' {
			i++
			continue
		}
		if i >= len(body) || body[i] != '}' {
			return nil, false
		}
		break
	}
	var selected json.RawMessage
	for idx := range keys {
		span := spans[idx]
		if !span.present {
			continue
		}
		raw := body[span.start:span.end]
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			continue
		}
		if trimmed[0] != '[' {
			return nil, false
		}
		if selected == nil {
			selected = raw
		}
	}
	return selected, selected != nil
}

// modelMemberSpan is one top-level object member's raw value span in the
// discovery body. A span is a subslice, never a copied value.
type modelMemberSpan struct {
	start   int
	end     int
	present bool
}

// modelJSONKeyEquals reports whether a quoted JSON object key decodes to key.
// The common unescaped valid-UTF-8 member compares in place with no
// allocation; escaped or invalid-UTF-8 members fall back to the standard
// decoder so key semantics stay exactly encoding/json's.
func modelJSONKeyEquals(rawKey []byte, key string) bool {
	inner := rawKey[1 : len(rawKey)-1]
	if bytes.IndexByte(inner, '\\') < 0 && utf8.Valid(inner) {
		return string(inner) == key
	}
	var decoded string
	if err := json.Unmarshal(rawKey, &decoded); err != nil {
		return false
	}
	return decoded == key
}

// checkModelArrayCount scans an entry array and fails closed as soon as it
// holds more than limit elements, so the caller never materializes the
// oversized []map[string]any. Elements themselves stay as raw spans; the
// count walk allocates only the small per-element raw buffer.
func checkModelArrayCount(raw []byte, limit int) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return fmt.Errorf("model entries are not an array")
	}
	n := 0
	for dec.More() {
		var elem json.RawMessage
		if err := dec.Decode(&elem); err != nil {
			return err
		}
		n++
		if n > limit {
			return errModelsDiscoveryEntries
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	return nil
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
		id, _ := e["id"].(string)
		if strings.TrimSpace(id) == "" {
			continue
		}
		// Fill-only backfill of the canonical base (format.NewModelEntry):
		// an upstream field always wins over the proxy's default, and id is
		// guaranteed present by the check above, so exactly object/created/
		// owned_by can be filled.
		for k, v := range providerformat.NewModelEntry(id, provider, 0) {
			if _, ok := e[k]; !ok {
				e[k] = v
			}
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
// already carry it (the upstream's own value always wins). The metadata source
// draws from the discovery-wide entry budget that the primary list already
// debited, so it can never hide a second 10,000-entry catalog. Any failure -
// fetch, status, shape, or an insufficient remaining entry budget - degrades
// to the unenriched base list: enrichment is a completeness win, never a hard
// dependency.
func (s *Server) enrichModelEntries(d *modelsDiscovery, t *target, key string, entries []map[string]any) {
	ov, ok := s.cfg().Providers[t.provider]
	if !ok || ov.ModelsPath == "" || len(ov.ModelsKeys) == 0 || len(entries) == 0 {
		return
	}
	body, err := s.fetchModelsUpstream(d, t, key, joinUpstreamURL(t.baseURL, ov.ModelsPath))
	if err != nil {
		return
	}
	src, ok, err := unwrapEntries(body, d.entriesLeft, "data", "models")
	if err != nil || !ok {
		return
	}
	// Charge the metadata lookups to the same discovery-wide budget before
	// building their map. unwrapEntries bounded the source by entriesLeft, so
	// this cannot fail once it returned successfully.
	if d.consumeEntries(len(src)) != nil {
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
		// Discovery uses the resolved target auth header and prefix, the same
		// construction as the relay. t.authHeader always carries a value
		// (Authorization unless the client names one), so Anthropic discovery
		// has no protocol-specific x-api-key fallback: an endpoint that
		// requires it needs X-Proxy-Auth-Header: x-api-key with an empty
		// prefix, exactly like a relayed request.
		if key != "" {
			req.Header.Set(t.authHeader, t.authPrefix+key)
		}
		req.Header.Set("anthropic-version", anthropicVersion)

		body, err := s.fetchModelsResponse(d, req, t, key)
		if err != nil {
			writeModelsError(w, err)
			return
		}
		// Count the page's data array before the typed decode: the upstream is
		// free to ignore the limit parameter, and the aggregate budget spans
		// every page.
		if raw, ok := selectModelArray(body, "data"); ok {
			if err := checkModelArrayCount(raw, d.entriesLeft-len(entries)); err != nil {
				if errors.Is(err, errModelsDiscoveryEntries) {
					writeModelsError(w, errModelsDiscoveryEntries)
					return
				}
			}
		}
		page, err := providerformat.TranslateAnthropicModels(body)
		if err != nil {
			writeModelsError(w, fmt.Errorf("decode anthropic models: %w", err))
			return
		}
		entries = append(entries, page.Entries...)
		if len(entries) > d.entriesLeft {
			writeModelsError(w, errModelsDiscoveryEntries)
			return
		}
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
