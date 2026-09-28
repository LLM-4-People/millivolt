package mcp

import (
	"context"
	"fmt"
	"strings"
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
	{"pause", "/admin/pause"},
	{"throttle", "/admin/throttle"},
	{"quota", "/admin/quota"},
	{"restart", "/admin/restart"},
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
	All       bool     `json:"all,omitempty" jsonschema:"hold all traffic; the proxy rejects a hold with no scope at all"`
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
	if err := s.client.postJSON(ctx, "/admin/pause", body, &state); err != nil {
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
	Provider      string `json:"provider" jsonschema:"the provider these limits apply to, across all clients and keys"`
	Clear         bool   `json:"clear,omitempty" jsonschema:"remove this provider's policy entirely"`
	Concurrency   *int   `json:"concurrency,omitempty" jsonschema:"simultaneous in-flight requests; 0 disables the dimension"`
	Requests      *int64 `json:"requests,omitempty" jsonschema:"requests per window; 0 disables the dimension"`
	RequestWindow string `json:"request_window,omitempty" jsonschema:"window for the request budget, for example 1m"`
	Tokens        *int64 `json:"tokens,omitempty" jsonschema:"tokens per window; 0 disables the dimension"`
	TokenWindow   string `json:"token_window,omitempty" jsonschema:"window for the token budget, for example 1m"`
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

// SetThrottle merges supplied dimensions into one provider's limits. An omitted
// dimension keeps its current value; 0 turns that dimension off.
func (s *Service) setThrottle(ctx context.Context, in SetThrottleInput) (*SetThrottleOutput, error) {
	if strings.TrimSpace(in.Provider) == "" {
		return nil, fmt.Errorf("provider is required")
	}
	body := map[string]any{"provider": in.Provider}
	if in.Clear {
		body["clear"] = true
	} else {
		if in.Concurrency != nil {
			if *in.Concurrency < 0 {
				return nil, fmt.Errorf("concurrency must be >= 0")
			}
			body["concurrency"] = *in.Concurrency
		}
		if in.Requests != nil {
			if *in.Requests < 0 {
				return nil, fmt.Errorf("requests must be >= 0")
			}
			body["requests"] = *in.Requests
		}
		if in.RequestWindow != "" {
			body["request_window"] = in.RequestWindow
		}
		if in.Tokens != nil {
			if *in.Tokens < 0 {
				return nil, fmt.Errorf("tokens must be >= 0")
			}
			body["tokens"] = *in.Tokens
		}
		if in.TokenWindow != "" {
			body["token_window"] = in.TokenWindow
		}
	}
	var state map[string]any
	if err := s.client.postJSON(ctx, "/admin/throttle", body, &state); err != nil {
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

// ResumeQuotaInput closes one provider's open retry-mode quota gate.
type ResumeQuotaInput struct {
	Provider string `json:"provider" jsonschema:"the provider whose quota gate should close"`
}

// ResumeQuotaOutput is the resulting storm/quota state.
type ResumeQuotaOutput struct {
	State   map[string]any `json:"state" jsonschema:"storm state after the resume"`
	Resumed bool           `json:"resumed" jsonschema:"true when an open gate was closed"`
	Note    string         `json:"note" jsonschema:"what this does and does not affect"`
	Warning string         `json:"warning,omitempty" jsonschema:"the proxy applied the state in memory but could not persist it"`
}

// quotaNote separates the retry-mode gate from a manual hold.
const quotaNote = "this closes a provider's open retry-mode quota gate so parked sends resume immediately; " +
	"a manual-mode quota hold resumes through the pause surface instead"

// ResumeQuota closes an open retry-mode quota gate. A provider with no open gate
// is a no-op state report: the gate may have recovered on its own.
func (s *Service) resumeQuota(ctx context.Context, in ResumeQuotaInput) (*ResumeQuotaOutput, error) {
	if strings.TrimSpace(in.Provider) == "" {
		return nil, fmt.Errorf("provider is required")
	}
	body := map[string]any{"provider": in.Provider, "resume": true}
	var state map[string]any
	if err := s.client.postJSON(ctx, "/admin/quota", body, &state); err != nil {
		return nil, err
	}
	out := &ResumeQuotaOutput{State: object(state), Note: quotaNote}
	if warning, ok := state["warning"].(string); ok {
		out.Warning = warning
	}
	if gates, ok := state["quota_gates"].([]any); ok {
		out.Resumed = !containsProvider(gates, in.Provider)
	}
	return out, nil
}

func containsProvider(gates []any, provider string) bool {
	for _, gate := range gates {
		document, ok := gate.(map[string]any)
		if !ok {
			continue
		}
		if name, ok := document["provider"].(string); ok && name == provider {
			return true
		}
	}
	return false
}

// SetConfigInput is a revision-checked partial patch of the config file.
type SetConfigInput struct {
	// Values carries ONLY the keys to change. A supplied list or map replaces
	// that whole field, so read the current document first for structured keys.
	Values map[string]any `json:"values" jsonschema:"only the keys to change; omitted keys are untouched"`
	// Revision must be the revision from a config read. Empty means "use the
	// current revision", which still fails closed against a concurrent write.
	Revision string `json:"revision,omitempty" jsonschema:"the revision returned by a previous read; a stale value is rejected with 409"`
}

// SetConfigOutput is the saved state.
type SetConfigOutput struct {
	Saved           bool           `json:"saved" jsonschema:"true when the config file was written"`
	RestartRequired []string       `json:"restart_required" jsonschema:"keys whose new value needs a process restart to take effect"`
	Values          map[string]any `json:"values,omitempty" jsonschema:"the saved config values"`
	Error           string         `json:"error,omitempty" jsonschema:"set when the file was saved but the reload failed; the file already changed"`
	Warning         string         `json:"warning,omitempty" jsonschema:"the proxy's last-reload warning"`
}

// configState is the subset of GET /admin/config this server reads. Only the
// revision is needed, and only when the caller did not supply one.
type configState struct {
	Revision        string   `json:"revision"`
	RestartRequired []string `json:"restart_required"`
}

// SetConfig applies a revision-checked partial patch. The proxy validates the
// whole result before writing, strips CLI-overridden keys, and reports
// restart-required keys; a failed reload still leaves the file written.
func (s *Service) setConfig(ctx context.Context, in SetConfigInput) (*SetConfigOutput, error) {
	if len(in.Values) == 0 {
		return nil, fmt.Errorf("values must name at least one key to change")
	}
	revision := in.Revision
	if revision == "" {
		var state configState
		if err := s.client.getJSON(ctx, "/admin/config", nil, &state); err != nil {
			return nil, fmt.Errorf("read the current config revision before patching: %v", err)
		}
		revision = state.Revision
		if revision == "" {
			return nil, fmt.Errorf("the proxy reported no config revision; the config file may not be writable")
		}
	}
	body := map[string]any{"values": in.Values, "revision": revision}
	var saved struct {
		Saved           bool           `json:"saved"`
		RestartRequired []string       `json:"restart_required"`
		Values          map[string]any `json:"values"`
		Error           string         `json:"error"`
	}
	if err := s.client.postJSON(ctx, "/admin/config", body, &saved); err != nil {
		return nil, err
	}
	out := &SetConfigOutput{
		Saved: saved.Saved, RestartRequired: list(saved.RestartRequired),
		Values: object(saved.Values), Error: saved.Error,
	}
	if len(out.RestartRequired) == 0 {
		out.RestartRequired = []string{}
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
	if err := s.client.postJSON(ctx, "/admin/reload", nil, &payload); err != nil {
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
	StatusCode     int    `json:"status_code,omitempty" jsonschema:"exact final HTTP status, 0 to 999"`
	HasError       bool   `json:"has_error,omitempty" jsonschema:"only requests that are errors under the canonical predicate"`
	HasDebug       bool   `json:"has_debug,omitempty" jsonschema:"only requests with a stored capture document"`
	BeforeMs       int64  `json:"before_ms,omitempty" jsonschema:"only rows with started_at before this, unix milliseconds"`
	AfterMs        int64  `json:"after_ms,omitempty" jsonschema:"only rows with started_at at or after this, unix milliseconds"`
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

// PurgePreviewOutput is the matched-row count.
type PurgePreviewOutput struct {
	Count    int64  `json:"count" jsonschema:"rows matching right now"`
	Filter   string `json:"filter" jsonschema:"the predicate that was counted"`
	Warning  string `json:"warning" jsonschema:"the count can still change before the deletion runs"`
	Verified bool   `json:"verified" jsonschema:"true when the same filter was accepted by the proxy"`
}

// purgePreviewWarning is the standing caveat: the preview is a read, not a lock.
const purgePreviewWarning = "traffic can change this count before a deletion runs; the filter is what is fixed, not the row set"

// PurgePreview counts the rows a purge filter would delete, without deleting
// anything.
func (s *Service) purgePreview(ctx context.Context, in PurgePreviewInput) (*PurgePreviewOutput, error) {
	if in.Filter.isEmpty() {
		return nil, fmt.Errorf("filter must constrain at least one field; this server never previews or sends the delete-everything command")
	}
	var payload struct {
		Count int64 `json:"count"`
	}
	if err := s.client.postJSON(ctx, "/admin/purge/count", in.Filter.document(), &payload); err != nil {
		return nil, err
	}
	return &PurgePreviewOutput{
		Count: payload.Count, Filter: in.Filter.describe(),
		Warning: purgePreviewWarning, Verified: true,
	}, nil
}

// PurgeInput deletes history irreversibly.
type PurgeInput struct {
	Filter PurgeFilterInput `json:"filter" jsonschema:"the predicate to delete; must constrain at least one field"`
	// Confirmation must equal PurgeConfirmation exactly. It cannot be satisfied
	// by a plausible-looking guess.
	Confirmation string `json:"confirmation" jsonschema:"must be the exact phrase 'permanently delete the matching millivolt history'"`
	// ReviewedCount must equal a purge_preview count for the SAME filter.
	ReviewedCount int64 `json:"reviewed_count" jsonschema:"the count purge_preview reported for this exact filter; run the preview first"`
}

// PurgeConfirmation is the literal a caller must echo. A purge is the only
// irreversible call this server makes, so it is gated on a phrase no
// plausible-but-wrong argument can produce, on a count that must have come from
// a real preview, and on a filter that cannot be empty.
const PurgeConfirmation = "permanently delete the matching millivolt history"

// PurgeOutput is the deletion outcome.
type PurgeOutput struct {
	Deleted      bool   `json:"deleted" jsonschema:"true when the proxy accepted the deletion"`
	Filter       string `json:"filter" jsonschema:"the predicate that was applied"`
	Reviewed     int64  `json:"reviewed_count" jsonschema:"the count the call was authorized against"`
	Recount      int64  `json:"remaining_count" jsonschema:"rows still matching right after the deletion"`
	Irreversible string `json:"irreversible" jsonschema:"what cannot be undone"`
	Note         string `json:"note" jsonschema:"what the deletion does not touch"`
}

// purgeIrreversible states the consequence in the output itself.
const purgeIrreversible = "deleted request history cannot be recovered from this API; only an operator backup taken beforehand can restore it"

// purgeNote states what survives, so a model does not over-promise.
const purgeNote = "the deletion preserves newer completions and in-flight work; it does not reset live error-storm protection, and it never touches stored debug capture documents"

// Purge deletes matching history. It is irreversible, has no server-side
// confirmation and no two-step protocol: the credential and this tool's own
// guards are the whole gate. The guard is threefold and each part alone is
// insufficient: an exact confirmation phrase, a reviewed count that must match
// a real preview of the same filter, and a filter that can never be empty. The
// delete-everything command (a bodyless request) is never sent by this tool.
func (s *Service) purge(ctx context.Context, in PurgeInput) (*PurgeOutput, error) {
	if in.Filter.isEmpty() {
		return nil, fmt.Errorf("filter must constrain at least one field: this server never sends the empty-body command that deletes ALL history")
	}
	if in.Confirmation != PurgeConfirmation {
		return nil, fmt.Errorf("confirmation must be the exact phrase %q; this call is irreversible and undoes itself by nothing", PurgeConfirmation)
	}
	if in.ReviewedCount < 0 {
		return nil, fmt.Errorf("reviewed_count must be the non-negative count purge_preview reported for this exact filter")
	}
	// Re-count before deleting, so a deletion cannot be authorized against a
	// preview of a different row set than the one being removed.
	var current struct {
		Count int64 `json:"count"`
	}
	if err := s.client.postJSON(ctx, "/admin/purge/count", in.Filter.document(), &current); err != nil {
		return nil, fmt.Errorf("verify the reviewed count before deleting: %v", err)
	}
	if current.Count != in.ReviewedCount {
		return nil, fmt.Errorf("the filter now matches %d rows but %d were reviewed: re-run purge_preview and confirm again",
			current.Count, in.ReviewedCount)
	}
	if err := s.client.postJSON(ctx, "/admin/purge", in.Filter.document(), nil); err != nil {
		return nil, err
	}
	out := &PurgeOutput{
		Deleted: true, Filter: in.Filter.describe(), Reviewed: in.ReviewedCount,
		Irreversible: purgeIrreversible, Note: purgeNote,
	}
	// The post-delete recount is informational: it never fails the call.
	var remaining struct {
		Count int64 `json:"count"`
	}
	if err := s.client.postJSON(ctx, "/admin/purge/count", in.Filter.document(), &remaining); err == nil {
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
