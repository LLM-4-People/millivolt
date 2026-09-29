package mcp

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// OperatorStateOutput reads every operator control in one call: the pause
// holds, the provider limits, the storm/quota state and the restart status.
// Four separate reads would be four round trips for one question.
type OperatorStateOutput struct {
	Pause    map[string]any `json:"pause" jsonschema:"pause holds, known clients/providers and the default queue cap"`
	Throttle map[string]any `json:"throttle" jsonschema:"active provider limits with in-flight and queued state"`
	Quota    map[string]any `json:"quota" jsonschema:"storm state, including open provider quota gates"`
	Restart  map[string]any `json:"restart" jsonschema:"self-restart eligibility and status"`
	Problems []string       `json:"problems,omitempty" jsonschema:"surfaces that could not be read, with the reason"`
}

// OperatorStateInput takes no arguments.
type OperatorStateInput struct{}

// operatorStateReads are the four GET surfaces, in a fixed order so the output
// is deterministic. Each is independent: one failure is reported in Problems
// while the rest still answer.
var operatorStateReads = []struct {
	name string
	path string
}{
	{"pause", routePause},
	{"throttle", routeThrottle},
	{"quota", routeQuota},
	{"restart", routeRestart},
}

// OperatorState reads pause, limits, quota/storm and restart status together.
func (s *Service) operatorState(ctx context.Context, _ OperatorStateInput) (*OperatorStateOutput, error) {
	out := &OperatorStateOutput{
		Pause:    map[string]any{},
		Throttle: map[string]any{},
		Quota:    map[string]any{},
		Restart:  map[string]any{},
		Problems: []string{},
	}
	values := map[string]*map[string]any{
		"pause": &out.Pause, "throttle": &out.Throttle,
		"quota": &out.Quota, "restart": &out.Restart,
	}
	var firstErr error
	for _, read := range operatorStateReads {
		var document map[string]any
		if err := s.client.getJSON(ctx, read.path, nil, &document); err != nil {
			out.Problems = append(out.Problems, read.name+": "+err.Error())
			if firstErr == nil {
				firstErr = fmt.Errorf("read %s: %v", read.name, err)
			}
			continue
		}
		*values[read.name] = document
	}
	if firstErr != nil && len(out.Problems) == len(operatorStateReads) {
		// Every surface failed: that is a connectivity or credential problem,
		// not four unrelated ones.
		return nil, firstErr
	}
	return out, nil
}

// SetPauseInput creates, edits or resumes pause holds.
type SetPauseInput struct {
	// Paused false resumes. With no HoldID it resumes EVERYTHING.
	Paused    bool     `json:"paused" jsonschema:"true parks matching new requests, false resumes"`
	HoldID    string   `json:"hold_id,omitempty" jsonschema:"edit or resume exactly this hold"`
	All       bool     `json:"all,omitempty" jsonschema:"hold ALL traffic. Naming no scope at all is the SAME global hold, so leave every scope field empty only when you mean everything"`
	New       bool     `json:"new,omitempty" jsonschema:"hold clients that were unseen when the hold was created"`
	Clients   []string `json:"clients,omitempty" jsonschema:"client scope; names within one dimension are alternatives"`
	Providers []string `json:"providers,omitempty" jsonschema:"provider scope; provider and client constraints intersect"`
	Duration  string   `json:"duration,omitempty" jsonschema:"15m, 1h, 6h, 12h, 24h, or empty until resumed"`
	MaxQueued *int     `json:"max_queued,omitempty" jsonschema:"how many matching waiters the hold parks before refusing"`
}

// SetPauseOutput is the resulting pause state.
type SetPauseOutput struct {
	State   map[string]any `json:"state" jsonschema:"the full pause state after the change"`
	Resumed bool           `json:"resumed" jsonschema:"true when this call resumed rather than created or edited"`
	Scope   string         `json:"scope" jsonschema:"the scope the hold now covers"`
	Note    string         `json:"note" jsonschema:"what pause does and does not do"`
	Warning string         `json:"warning,omitempty" jsonschema:"the proxy applied the state in memory but could not persist it"`
}

// pauseNote states what a hold actually does, so a model does not promise a
// cancellation it cannot deliver.
const pauseNote = "a hold parks matching NEW sends and retries; in-flight requests are never cancelled, and excess waiting work can be refused"

// SetPause creates, edits or resumes pause holds. In-flight requests always
// finish; this never disconnects them.
func (s *Service) setPause(ctx context.Context, in SetPauseInput) (*SetPauseOutput, error) {
	body := map[string]any{"paused": in.Paused}
	if in.HoldID != "" {
		body["id"] = in.HoldID
	}
	if in.Paused {
		if in.Duration != "" {
			if err := validateCaptureDuration(in.Duration); err != nil {
				return nil, fmt.Errorf("duration: %v", err)
			}
			body["duration"] = in.Duration
		}
		if in.All {
			body["all"] = true
		}
		if in.New {
			body["new"] = true
		}
		if in.Clients != nil {
			body["clients"] = in.Clients
		}
		if in.Providers != nil {
			body["providers"] = in.Providers
		}
		if in.MaxQueued != nil {
			if *in.MaxQueued < 0 {
				return nil, fmt.Errorf("max_queued must be >= 0")
			}
			body["max_queued"] = *in.MaxQueued
		}
	}
	var state map[string]any
	if err := s.client.postJSON(ctx, routePause, body, &state); err != nil {
		return nil, err
	}
	out := &SetPauseOutput{State: object(state), Resumed: !in.Paused, Note: pauseNote}
	if warning, ok := state["warning"].(string); ok {
		out.Warning = warning
	}
	out.Scope = describePauseScope(in)
	return out, nil
}

func describePauseScope(in SetPauseInput) string {
	if !in.Paused {
		if in.HoldID != "" {
			return "resumed hold " + in.HoldID
		}
		return "resumed every hold"
	}
	parts := []string{}
	if in.All || (in.HoldID == "" && !in.New && len(in.Clients) == 0 && len(in.Providers) == 0) {
		parts = append(parts, "all traffic")
	}
	if in.New {
		parts = append(parts, "clients unseen at hold creation")
	}
	if len(in.Clients) > 0 {
		parts = append(parts, "clients "+strings.Join(in.Clients, ","))
	}
	if len(in.Providers) > 0 {
		parts = append(parts, "providers "+strings.Join(in.Providers, ","))
	}
	if len(parts) == 0 {
		if in.HoldID != "" {
			return "edited hold " + in.HoldID + " in place"
		}
		return "no scope"
	}
	scope := strings.Join(parts, " and ")
	if in.HoldID != "" {
		return "edited hold " + in.HoldID + ": " + scope
	}
	return scope
}

// SetThrottleInput sets or clears one provider's limits.
type SetThrottleInput struct {
	Provider      string `json:"provider" jsonschema:"the provider these limits apply to, across all clients and keys; must already be a known provider name"`
	Clear         bool   `json:"clear,omitempty" jsonschema:"remove this provider's policy entirely"`
	Concurrency   *int   `json:"concurrency,omitempty" jsonschema:"simultaneous in-flight requests, 0 to 100000; 0 disables the dimension"`
	Requests      *int64 `json:"requests,omitempty" jsonschema:"requests per window, 0 to 1000000000; 0 disables the dimension"`
	RequestWindow string `json:"request_window,omitempty" jsonschema:"window for the request budget, 1s to 24h, for example 1m or 2h; required when a count is set and the provider has no window yet"`
	Tokens        *int64 `json:"tokens,omitempty" jsonschema:"tokens per window, 0 to 1000000000000; 0 disables the dimension"`
	TokenWindow   string `json:"token_window,omitempty" jsonschema:"window for the token budget, 1s to 24h, for example 1m or 2h; required when a count is set and the provider has no window yet"`
}

// SetThrottleOutput is the resulting limits state.
type SetThrottleOutput struct {
	State          map[string]any `json:"state" jsonschema:"every active provider limit after the change"`
	KnownProviders []string       `json:"known_providers" jsonschema:"providers a limit can be set on"`
	Note           string         `json:"note" jsonschema:"what these limits do and do not enforce"`
	Warning        string         `json:"warning,omitempty" jsonschema:"the proxy applied the state in memory but could not persist it"`
}

// throttleNote states the enforcement semantics honestly.
const throttleNote = "these are provider-wide budgets that permit bursts and oversized requests; they are not strict fixed-window quota enforcement"

// The bands the proxy enforces on a provider limit (internal/proxy/throttle.go
// maxLimit* and min/maxLimitWindow). Applying them here turns a typo into a
// readable message instead of a 400, and stops a request the proxy would refuse
// after it was already assembled.
const (
	maxLimitConcurrency = 100_000
	maxLimitRequests    = 1_000_000_000
	maxLimitTokens      = 1_000_000_000_000
	minLimitWindow      = time.Second
	maxLimitWindow      = 24 * time.Hour
)

// throttleWindow parses a limit window the way the proxy's parseLimitWindow
// does: a Go duration, or an N/1d/2d form it converts to 24-hour units, then the
// 1s..24h band.
func throttleWindow(raw string) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, fmt.Errorf("window required")
	}
	var window time.Duration
	if len(trimmed) > 1 && (trimmed[len(trimmed)-1] == 'd' || trimmed[len(trimmed)-1] == 'D') {
		days, err := strconv.ParseFloat(trimmed[:len(trimmed)-1], 64)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid window %q", raw)
		}
		window = time.Duration(days * float64(24*time.Hour))
	} else {
		parsed, err := time.ParseDuration(trimmed)
		if err != nil {
			return 0, fmt.Errorf("invalid window %q", raw)
		}
		window = parsed
	}
	if window < minLimitWindow || window > maxLimitWindow {
		return 0, fmt.Errorf("window must be %s..%s, got %q", minLimitWindow, maxLimitWindow, raw)
	}
	return window, nil
}

// SetThrottle merges supplied dimensions into one provider's limits. An omitted
// dimension keeps its current value; 0 turns that dimension off.
//
// The provider name is checked against the operator's own known vocabulary
// first. The proxy ADDS an unknown provider to that vocabulary when a limit is
// set on it, so a typo would not fail - it would permanently pollute the set
// every later validation and every discovery list is built from.
func (s *Service) setThrottle(ctx context.Context, in SetThrottleInput) (*SetThrottleOutput, error) {
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		return nil, fmt.Errorf("provider is required")
	}
	body := map[string]any{"provider": in.Provider}
	if in.Clear {
		body["clear"] = true
	} else {
		if in.Concurrency != nil {
			if *in.Concurrency < 0 || *in.Concurrency > maxLimitConcurrency {
				return nil, fmt.Errorf("concurrency must be 0..%d", maxLimitConcurrency)
			}
			body["concurrency"] = *in.Concurrency
		}
		if in.Requests != nil {
			if *in.Requests < 0 || *in.Requests > maxLimitRequests {
				return nil, fmt.Errorf("requests must be 0..%d", maxLimitRequests)
			}
			body["requests"] = *in.Requests
		}
		if in.RequestWindow != "" {
			if _, err := throttleWindow(in.RequestWindow); err != nil {
				return nil, fmt.Errorf("request_window: %v", err)
			}
			body["request_window"] = in.RequestWindow
		}
		if in.Tokens != nil {
			if *in.Tokens < 0 || *in.Tokens > maxLimitTokens {
				return nil, fmt.Errorf("tokens must be 0..%d", maxLimitTokens)
			}
			body["tokens"] = *in.Tokens
		}
		if in.TokenWindow != "" {
			if _, err := throttleWindow(in.TokenWindow); err != nil {
				return nil, fmt.Errorf("token_window: %v", err)
			}
			body["token_window"] = in.TokenWindow
		}
		// A window with no count sets nothing and the proxy still answers 200,
		// which reads as a successful change. Refuse it here instead: a change
		// that sets nothing must not look like one that did.
		if in.Concurrency == nil && in.Requests == nil && in.Tokens == nil {
			return nil, fmt.Errorf("send the count the window applies to, or concurrency: " +
				"a window on its own sets nothing and the proxy would still answer 200")
		}
	}
	if err := s.requireKnownProvider(ctx, provider); err != nil {
		return nil, err
	}
	var state map[string]any
	if err := s.client.postJSON(ctx, routeThrottle, body, &state); err != nil {
		return nil, err
	}
	out := &SetThrottleOutput{State: object(state), KnownProviders: []string{}, Note: throttleNote}
	if known, ok := state["known_providers"].([]any); ok {
		out.KnownProviders = anyStrings(known)
	}
	if warning, ok := state["warning"].(string); ok {
		out.Warning = warning
	}
	return out, nil
}

// requireKnownProvider refuses a provider name outside the operator's own
// vocabulary, offering the known names. The proxy records the name on ANY
// accepted throttle write, set or clear, so a typo would not fail - it would
// permanently pollute the set every later validation and every discovery list
// is built from.
//
// This is deliberately not a hard guard: it fails OPEN on a debug-read error
// and on an empty vocabulary, so an unknown name may be accepted when the
// vocabulary is unavailable. Refusing then would block a legitimate change on
// an unrelated failure, and the output still reports the vocabulary the proxy
// ended up with.
func (s *Service) requireKnownProvider(ctx context.Context, provider string) error {
	// GET /admin/debug is the one owner of the names an operator may scope to.
	status, err := s.client.debug(ctx)
	if err != nil {
		return nil
	}
	known := list(status.KnownProviders)
	if len(known) == 0 || slices.Contains(known, provider) {
		return nil
	}
	return fmt.Errorf("provider %q is not one of the known providers, and writing a limit for it would add the name to the proxy's vocabulary permanently. "+
		"Known providers: %s", provider, strings.Join(known, ", "))
}

// ResumeQuotaInput closes one provider's open retry-mode quota gate.
type ResumeQuotaInput struct {
	Provider string `json:"provider" jsonschema:"the provider whose quota gate should close"`
}

// ResumeQuotaOutput is the resulting storm/quota state.
type ResumeQuotaOutput struct {
	State   map[string]any `json:"state" jsonschema:"storm state after the resume"`
	Resumed bool           `json:"resumed" jsonschema:"true when no open quota gate remains for this provider"`
	Open    []string       `json:"open_gates" jsonschema:"providers that still have an open retry-mode quota gate"`
	Note    string         `json:"note" jsonschema:"what this does and does not affect"`
	Warning string         `json:"warning,omitempty" jsonschema:"the proxy applied the state in memory but could not persist it"`
}

// quotaNote separates the retry-mode gate from a manual hold.
const quotaNote = "this closes a provider's open retry-mode quota gate so parked sends resume immediately; " +
	"a manual-mode quota hold resumes through the pause surface instead"

// openQuotaGates reads the open retry-mode quota gates out of the storm document
// /admin/quota actually returns: {enabled, banner_enabled, storms}, where a
// provider's gate is the "quota" flag on its entry inside "storms". There is no
// top-level "quota_gates" list on this route; reading one made "resumed"
// permanently false, which is a confidently wrong answer on every call.
func openQuotaGates(state map[string]any) []string {
	storms, ok := state["storms"].([]any)
	if !ok {
		return []string{}
	}
	open := []string{}
	for _, entry := range storms {
		document, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if gate, _ := document["quota"].(bool); !gate {
			continue
		}
		if provider, _ := document["provider"].(string); provider != "" {
			open = append(open, provider)
		}
	}
	return open
}

// ResumeQuota closes an open retry-mode quota gate. A provider with no open gate
// is a no-op state report: the gate may have recovered on its own.
func (s *Service) resumeQuota(ctx context.Context, in ResumeQuotaInput) (*ResumeQuotaOutput, error) {
	// Trim ONCE and use the trimmed value everywhere: the proxy compares the
	// provider verbatim (internal/proxy/quotapause.go), so a padded name would
	// reach a different gate than the response check below, and
	// resume_quota(" local ") would report resumed=true while the real gate
	// stayed open.
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		return nil, fmt.Errorf("provider is required")
	}
	body := map[string]any{"provider": provider, "resume": true}
	var state map[string]any
	if err := s.client.postJSON(ctx, routeQuota, body, &state); err != nil {
		return nil, err
	}
	out := &ResumeQuotaOutput{State: object(state), Open: []string{}, Note: quotaNote}
	if warning, ok := state["warning"].(string); ok {
		out.Warning = warning
	}
	out.Open = openQuotaGates(state)
	out.Resumed = !slices.Contains(out.Open, provider)
	return out, nil
}

// ConfigGetInput takes no arguments: the document is a property of the running
// proxy, not of a request.
type ConfigGetInput struct{}

// ConfigGetOutput is GET /admin/config: the one owner of that document's shape.
// set_config reads the same shape, so the two can never disagree about which
// key carries the revision.
type ConfigGetOutput struct {
	// Values are the config FILE's values. A supplied list or map in set_config
	// replaces that whole field, so this is what a structured key must be read
	// from before it is patched.
	Values map[string]any `json:"values" jsonschema:"the config file's current values"`
	// Effective are the RUNNING values, after CLI overrides. A key can differ
	// from Values when the process was started with a flag that overrides it.
	Effective map[string]any `json:"effective" jsonschema:"the values in force now, with CLI overrides applied"`
	// Defaults make an unchanged key recognizable: a value equal to the default
	// is a deliberate setting, not an absent one.
	Defaults map[string]any `json:"defaults" jsonschema:"built-in defaults"`
	// Fields is the schema: one descriptor per key with its type, category and
	// documentation, which is what makes a patch well typed.
	Fields     any `json:"fields" jsonschema:"the field schema: type, category and documentation per key"`
	Categories any `json:"categories,omitempty" jsonschema:"the field categories the dashboard groups by"`
	// RestartRequired names the keys whose value needs a process restart, not
	// just a reload, to take effect.
	RestartRequired []string `json:"restart_required" jsonschema:"keys whose value needs a process restart to take effect"`
	// Revision is what set_config echoes back so a concurrent write is rejected
	// instead of overwritten.
	Revision string `json:"revision" jsonschema:"pass this to set_config; a stale value is rejected with 409"`
	// Writable is false when the proxy was started with no -config path, in
	// which case set_config has nowhere to persist and will be refused.
	Writable   bool           `json:"writable" jsonschema:"false when the proxy has no config file, so set_config cannot persist"`
	Path       string         `json:"path,omitempty" jsonschema:"the config file this document came from"`
	Backup     map[string]any `json:"backup,omitempty" jsonschema:"config and database backup availability and policy"`
	LastReload any            `json:"last_reload,omitempty" jsonschema:"outcome of the most recent configuration application"`
	Note       string         `json:"note" jsonschema:"how to use this document with set_config"`
}

// configNote is the workflow the set_config description points at.
const configNote = "patch with set_config by sending only the keys to change, plus this revision; " +
	"a supplied list or map replaces that whole field, so read it here first. Keys equal to their default are still real settings"

// ConfigGet returns the live configuration document: the values, the running
// overrides, the defaults, the per-key schema, the restart-required set and the
// revision a revision-checked patch must echo.
func (s *Service) configGet(ctx context.Context, _ ConfigGetInput) (*ConfigGetOutput, error) {
	var document ConfigGetOutput
	if err := s.client.getJSON(ctx, routeConfig, nil, &document); err != nil {
		return nil, err
	}
	document.Values = object(document.Values)
	document.Effective = object(document.Effective)
	document.Defaults = object(document.Defaults)
	document.RestartRequired = list(document.RestartRequired)
	document.Note = configNote
	return &document, nil
}

// SetConfigInput is a revision-checked partial patch of the config file.
type SetConfigInput struct {
	// Values carries ONLY the keys to change. A supplied list or map replaces
	// that whole field, so read the current document first with config_get for
	// structured keys.
	Values map[string]any `json:"values" jsonschema:"only the keys to change; omitted keys are untouched"`
	// Revision must be the revision from a config read. Empty means "use the
	// current revision", which still fails closed against a concurrent write.
	Revision string `json:"revision,omitempty" jsonschema:"the revision config_get reported; a stale value is rejected with 409"`
}

// SetConfigOutput is the saved state.
type SetConfigOutput struct {
	Saved           bool           `json:"saved" jsonschema:"true when the config file was written"`
	RestartRequired []string       `json:"restart_required" jsonschema:"keys whose new value needs a process restart to take effect"`
	Values          map[string]any `json:"values,omitempty" jsonschema:"the saved config values"`
	Error           string         `json:"error,omitempty" jsonschema:"set when the file was saved but the reload failed; the file already changed"`
	Warning         string         `json:"warning,omitempty" jsonschema:"the proxy's last-reload warning"`
}

// configPatch is the POST /admin/config response body, and the single owner of
// that shape. It is also decoded on a 500, because the proxy writes the file
// and only then reports a failed reload.
type configPatch struct {
	Saved           bool           `json:"saved"`
	Revision        string         `json:"revision"`
	RestartRequired []string       `json:"restart_required"`
	Values          map[string]any `json:"values"`
	// Error is the proxy's flat failure text as well as its saved-but-not-
	// reloaded text, because both arrive in the same field.
	Error string `json:"error"`
}

// savedConfigStatuses are the non-2xx statuses on this one route that still
// carry the committed document. 500 is the proxy's saved-but-not-reloaded
// answer; 409 (a stale revision) and 401/403 are ordinary failures and stay
// errors, so a model never mistakes a rejection for a save.
var savedConfigStatuses = map[int]bool{http.StatusInternalServerError: true}

// SetConfig applies a revision-checked partial patch. The proxy validates the
// whole result before writing, strips CLI-overridden keys, and reports
// restart-required keys; a failed reload still leaves the file written, and
// that is reported as a save, not as an error.
func (s *Service) setConfig(ctx context.Context, in SetConfigInput) (*SetConfigOutput, error) {
	if len(in.Values) == 0 {
		return nil, fmt.Errorf("values must name at least one key to change")
	}
	revision := in.Revision
	if revision == "" {
		var current ConfigGetOutput
		if err := s.client.getJSON(ctx, routeConfig, nil, &current); err != nil {
			return nil, fmt.Errorf("read the current config revision before patching: %v", err)
		}
		revision = current.Revision
		if revision == "" {
			return nil, fmt.Errorf("the proxy reported no config revision; the config file may not be writable")
		}
	}
	body := map[string]any{"values": in.Values, "revision": revision}
	var saved configPatch
	status, err := s.client.doJSONTolerating(ctx, http.MethodPost, routeConfig, nil, body, &saved, savedConfigStatuses)
	if err != nil {
		return nil, err
	}
	if !saved.Saved {
		// A tolerated status that reports no save is an ordinary failure whose
		// .error text decoded into the same field; report it as one. The text
		// is redacted like every other model-visible string on this path: the
		// proxy's reload failure can quote the request, credential included.
		return nil, &APIError{Status: status, Message: s.client.redact(errorMessage(saved.Error, status))}
	}
	out := &SetConfigOutput{
		Saved: saved.Saved, RestartRequired: list(saved.RestartRequired),
		Values: object(saved.Values), Error: s.client.redact(saved.Error),
	}
	if out.Error != "" {
		out.Warning = "the config file was written but the reload failed: " + out.Error
	}
	return out, nil
}

// ReloadConfigInput takes no arguments.
type ReloadConfigInput struct{}

// ReloadConfigOutput is the reload outcome.
type ReloadConfigOutput struct {
	OK              bool     `json:"ok" jsonschema:"true when the config file was re-read"`
	RestartRequired []string `json:"restart_required" jsonschema:"keys that changed but stay at their boot value until a restart"`
}

// ReloadConfig re-reads the config file. This is the one state-changing route
// the dashboard never calls.
func (s *Service) reloadConfig(ctx context.Context, _ ReloadConfigInput) (*ReloadConfigOutput, error) {
	var payload struct {
		OK              bool     `json:"ok"`
		RestartRequired []string `json:"restart_required"`
	}
	if err := s.client.postJSON(ctx, routeReload, nil, &payload); err != nil {
		return nil, err
	}
	return &ReloadConfigOutput{OK: payload.OK, RestartRequired: list(payload.RestartRequired)}, nil
}

// PurgeFilterInput is the shared export/delete/count predicate. Every field is
// an exact match on a stored value; omitted fields do not constrain.
type PurgeFilterInput struct {
	Provider       string `json:"provider,omitempty" jsonschema:"exact provider label"`
	Model          string `json:"model,omitempty" jsonschema:"exact stored model name, never a canonicalized one"`
	Client         string `json:"client,omitempty" jsonschema:"exact client classification"`
	ConversationID string `json:"conversation_id,omitempty" jsonschema:"exact conversation id"`
	ErrorType      string `json:"error_type,omitempty" jsonschema:"exact final error type"`
	StatusCode     int    `json:"status_code,omitempty" jsonschema:"exact final HTTP status, 0 to 999; 0 omits the constraint"`
	HasError       bool   `json:"has_error,omitempty" jsonschema:"only requests that are errors under the canonical predicate"`
	HasDebug       bool   `json:"has_debug,omitempty" jsonschema:"only requests with a stored capture document; their capture documents are deleted too"`
	BeforeMs       int64  `json:"before_ms,omitempty" jsonschema:"only rows with started_at before this, unix milliseconds, 0 or greater"`
	AfterMs        int64  `json:"after_ms,omitempty" jsonschema:"only rows with started_at at or after this, unix milliseconds; must be strictly less than before_ms, and 0 or greater"`
}

// purgeStatusCodeMax mirrors the proxy's own status_code band
// (internal/storage/store.go PurgeFilter.Validate), so a value the proxy would
// reject is a corrective message here instead of a raw 400.
const purgeStatusCodeMax = 999

// Validate applies the proxy's purge-filter rules client-side: the same status
// band, the same non-negative time bounds, and the same window ordering. It is
// the same validation the proxy's strict administrative decoder applies, so
// this is a fast, readable pre-check, never a second boundary.
func (f PurgeFilterInput) Validate() error {
	if f.StatusCode < 0 || f.StatusCode > purgeStatusCodeMax {
		return fmt.Errorf("status_code must be 0..%d", purgeStatusCodeMax)
	}
	if f.BeforeMs < 0 || f.AfterMs < 0 {
		return fmt.Errorf("before_ms and after_ms must be 0 or greater, unix milliseconds")
	}
	if f.BeforeMs != 0 && f.AfterMs >= f.BeforeMs {
		return fmt.Errorf("after_ms must be strictly less than before_ms: the window is (after_ms, before_ms)")
	}
	return nil
}

// isEmpty reports whether the filter constrains nothing. An empty filter is the
// delete-everything command, and this server refuses to send it.
func (f PurgeFilterInput) isEmpty() bool {
	return f.Provider == "" && f.Model == "" && f.Client == "" && f.ConversationID == "" &&
		f.ErrorType == "" && f.StatusCode == 0 && !f.HasError && !f.HasDebug &&
		f.BeforeMs == 0 && f.AfterMs == 0
}

// document renders the filter in the proxy's exact wire shape. Zero values are
// omitted so the proxy reads them as "no constraint", never as a match on 0.
func (f PurgeFilterInput) document() map[string]any {
	document := map[string]any{}
	if f.Provider != "" {
		document["provider"] = f.Provider
	}
	if f.Model != "" {
		document["model"] = f.Model
	}
	if f.Client != "" {
		document["client"] = f.Client
	}
	if f.ConversationID != "" {
		document["conversation_id"] = f.ConversationID
	}
	if f.ErrorType != "" {
		document["error_type"] = f.ErrorType
	}
	if f.StatusCode != 0 {
		document["status_code"] = f.StatusCode
	}
	if f.HasError {
		document["has_error"] = true
	}
	if f.HasDebug {
		document["debug"] = true
	}
	if f.BeforeMs != 0 {
		document["before_ms"] = f.BeforeMs
	}
	if f.AfterMs != 0 {
		document["after_ms"] = f.AfterMs
	}
	return document
}

func (f PurgeFilterInput) describe() string {
	parts := []string{}
	for key, value := range f.document() {
		parts = append(parts, fmt.Sprintf("%s=%v", key, value))
	}
	return strings.Join(parts, " ")
}

// PurgePreviewInput previews a deletion.
type PurgePreviewInput struct {
	Filter PurgeFilterInput `json:"filter" jsonschema:"the same predicate purge would use"`
}

// PurgePreviewOutput is the matched-row count plus the authorization for the
// matching deletion.
type PurgePreviewOutput struct {
	Count int64 `json:"count" jsonschema:"rows matching right now"`
	// PreviewToken is what authorizes the matching deletion. It is opaque and
	// carries the canonicalized filter, the count and an expiry, so a purge
	// cannot be authorized against a preview of a different row set - which a
	// bare count could not prevent, because two different filters can match the
	// same number of rows.
	PreviewToken string `json:"preview_token" jsonschema:"pass verbatim to purge; it authorizes THIS filter at THIS count, and expires"`
	Filter       string `json:"filter" jsonschema:"the predicate that was counted"`
	Warning      string `json:"warning" jsonschema:"what the preview does and does not fix"`
	Captures     string `json:"captures" jsonschema:"whether this filter also removes stored capture documents"`
	Verified     bool   `json:"verified" jsonschema:"true when the same filter was accepted by the proxy"`
}

// purgePreviewWarning is the standing caveat, stated narrowly on purpose. The
// re-check and the delete are two HTTP requests with no shared transaction, so
// live traffic committed between them is removed without having been previewed.
// No client-side guard closes that window, and claiming one would be a lie the
// operator discovers only after the rows are gone.
const purgePreviewWarning = "the preview authorizes this filter at this count, and the count is re-checked immediately before deleting; " +
	"live traffic committed between that re-check and the deletion is removed without having been previewed, " +
	"because the two are separate requests with no shared transaction"

// PurgePreview counts the rows a purge filter would delete, without deleting
// anything, and issues the token that authorizes exactly that deletion.
func (s *Service) purgePreview(ctx context.Context, in PurgePreviewInput) (*PurgePreviewOutput, error) {
	if in.Filter.isEmpty() {
		return nil, fmt.Errorf("filter must constrain at least one field; this server never previews or sends the delete-everything command")
	}
	if err := in.Filter.Validate(); err != nil {
		return nil, fmt.Errorf("filter: %v", err)
	}
	var payload struct {
		Count int64 `json:"count"`
	}
	if err := s.client.postJSON(ctx, routePurgeCount, in.Filter.document(), &payload); err != nil {
		return nil, err
	}
	token, err := s.previewToken.issue(in.Filter, payload.Count, time.Now())
	if err != nil {
		return nil, err
	}
	return &PurgePreviewOutput{
		Count: payload.Count, Filter: in.Filter.describe(), Warning: purgePreviewWarning,
		Captures: captureEffect(in.Filter), PreviewToken: token, Verified: true,
	}, nil
}

// PurgeInput deletes history irreversibly.
type PurgeInput struct {
	Filter PurgeFilterInput `json:"filter" jsonschema:"the predicate to delete; must constrain at least one field"`
	// Confirmation must equal PurgeConfirmation exactly. It cannot be satisfied
	// by a plausible-looking guess.
	Confirmation string `json:"confirmation" jsonschema:"must be the exact phrase 'permanently delete the matching millivolt history'"`
	// PreviewToken must be the token purge_preview issued for THIS filter. It is
	// required, not optional: without it a deletion can be authorized against a
	// row set that was never previewed, and a count alone cannot bind two
	// different filters that happen to match the same number of rows.
	PreviewToken string `json:"preview_token" jsonschema:"the preview_token purge_preview returned for this exact filter; it expires"`
}

// PurgeConfirmation is the literal a caller must echo. A purge is the only
// irreversible call this server makes, so it is gated on a phrase no
// plausible-but-wrong argument can produce, on a token that proves a real
// preview of the SAME filter produced it, and on a filter that cannot be empty.
const PurgeConfirmation = "permanently delete the matching millivolt history"

// PurgeOutput is the deletion outcome.
type PurgeOutput struct {
	Deleted      bool   `json:"deleted" jsonschema:"true when the proxy accepted the deletion"`
	Filter       string `json:"filter" jsonschema:"the predicate that was applied"`
	Reviewed     int64  `json:"reviewed_count" jsonschema:"the count carried by the preview token this call was authorized against"`
	Recount      int64  `json:"remaining_count" jsonschema:"rows still matching right after the deletion"`
	Irreversible string `json:"irreversible" jsonschema:"what cannot be undone"`
	Captures     string `json:"captures" jsonschema:"what this deletion did to stored capture documents"`
	Note         string `json:"note" jsonschema:"what the deletion does not touch"`
}

// purgeIrreversible states the consequence in the output itself.
const purgeIrreversible = "deleted request history cannot be recovered from this API; only an operator backup taken beforehand can restore it"

// purgeNote states what survives, so a model does not over-promise.
const purgeNote = "the deletion preserves newer completions and in-flight work, and it does not reset live error-storm protection"

// captureEffect states, per filter, whether stored capture documents go too.
// The proxy deletes the request_debug rows of every matching request in the
// same transaction (internal/storage/purge.go), so a filter that reaches a
// captured request takes its capture with it. Stating the opposite would lose an
// operator their evidence.
func captureEffect(filter PurgeFilterInput) string {
	if filter.HasDebug {
		return "this filter explicitly selects captured requests, so their stored capture documents are deleted with them"
	}
	return "every matching request's stored capture document is deleted with it; only a filter that provably cannot reach a captured request " +
		"would spare them, and this server cannot know which those are. Take a backup first if you still need the evidence"
}

// Purge deletes matching history. It is irreversible, has no server-side
// confirmation and no two-step protocol: the credential and this tool's own
// guards are the whole gate. The guard is threefold and each part alone is
// insufficient: an exact confirmation phrase, a preview token that binds THIS
// filter to a real preview of it, and a filter that can never be empty. The
// delete-everything command (a bodyless request) is never sent by this tool.
func (s *Service) purge(ctx context.Context, in PurgeInput) (*PurgeOutput, error) {
	if in.Filter.isEmpty() {
		return nil, fmt.Errorf("filter must constrain at least one field: this server never sends the empty-body command that deletes ALL history")
	}
	if err := in.Filter.Validate(); err != nil {
		return nil, fmt.Errorf("filter: %v", err)
	}
	if in.Confirmation != PurgeConfirmation {
		return nil, fmt.Errorf("confirmation must be the exact phrase %q; this call is irreversible and undoes itself by nothing", PurgeConfirmation)
	}
	reviewed, err := s.previewToken.verify(in.PreviewToken, in.Filter, time.Now())
	if err != nil {
		return nil, err
	}
	// Re-count before deleting, so a row set that visibly moved since the
	// preview refuses the call. It does NOT close the check/delete gap: rows
	// committed after this count are still removed.
	var current struct {
		Count int64 `json:"count"`
	}
	if err := s.client.postJSON(ctx, routePurgeCount, in.Filter.document(), &current); err != nil {
		return nil, fmt.Errorf("verify the reviewed count before deleting: %v", err)
	}
	if current.Count != reviewed {
		// Deliberately no numbers. A refusal that reports the live count is a
		// count oracle: a model calls purge again and the second call deletes.
		return nil, fmt.Errorf("the row set for this filter no longer matches the one purge_preview reviewed, so the deletion is refused. " +
			"Re-run purge_preview and confirm again against the count it reports")
	}
	if err := s.client.postJSON(ctx, routePurge, in.Filter.document(), nil); err != nil {
		return nil, err
	}
	out := &PurgeOutput{
		Deleted: true, Filter: in.Filter.describe(), Reviewed: reviewed,
		Irreversible: purgeIrreversible, Note: purgeNote, Captures: captureEffect(in.Filter),
	}
	// The post-delete recount is informational: it never fails the call.
	var remaining struct {
		Count int64 `json:"count"`
	}
	if err := s.client.postJSON(ctx, routePurgeCount, in.Filter.document(), &remaining); err == nil {
		out.Recount = remaining.Count
	}
	return out, nil
}

// anyStrings converts a decoded JSON string array.
func anyStrings(values []any) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			out = append(out, text)
		}
	}
	return out
}
