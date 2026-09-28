package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// AuditStatusOutput is the capture subsystem's current state.
type AuditStatusOutput struct {
	Enabled        bool           `json:"enabled" jsonschema:"true when at least one capture session is live"`
	Sessions       []AuditSession `json:"sessions" jsonschema:"live sessions, each with its scope, deadline and live capture count"`
	KnownClients   []string       `json:"known_clients" jsonschema:"valid client scope values"`
	KnownProviders []string       `json:"known_providers" jsonschema:"valid provider scope values"`
	KnownModels    []string       `json:"known_models" jsonschema:"valid model scope values (native normalization applies)"`
	Until          *string        `json:"until,omitempty" jsonschema:"soonest session deadline, RFC 3339"`
	TTL            string         `json:"capture_ttl" jsonschema:"how long a stored capture document is retained after the request"`
	MaxBytes       string         `json:"capture_max_bytes" jsonschema:"per-capture byte budget for request plus response bodies"`
	Storage        StorageInfo    `json:"storage" jsonschema:"durable storage signal; capture cannot work when it is false"`
	Warning        string         `json:"warning,omitempty" jsonschema:"the proxy applied the state in memory but could not persist it"`
	Retention      string         `json:"retention" jsonschema:"what stopping a session does and does not delete"`
}

// AuditSession is one live capture session.
type AuditSession struct {
	ID        string   `json:"id" jsonschema:"session id, used to edit or stop exactly this session"`
	Clients   []string `json:"clients" jsonschema:"client scope, empty meaning any client"`
	Providers []string `json:"providers" jsonschema:"provider scope, empty meaning any provider"`
	Models    []string `json:"models" jsonschema:"model scope, empty meaning any model"`
	Duration  string   `json:"duration" jsonschema:"the session's duration token"`
	Until     *string  `json:"until,omitempty" jsonschema:"absolute deadline, RFC 3339"`
	RanMs     int64    `json:"ran_ms" jsonschema:"how long the session has been running"`
	Captures  int64    `json:"captures" jsonschema:"live count of documents stored for this session"`
}

// retentionNote is the one statement of what capture stop does and does not do.
// It is repeated in the audit_start/audit_stop descriptions because the
// distinction is the one an operator is most likely to get wrong.
const retentionNote = "stopping a session does NOT delete captures that were already stored: there is no delete " +
	"endpoint, and a stopped session's documents stay readable by record id until debug_capture_ttl expires"

// AuditStatusInput takes no arguments.
type AuditStatusInput struct{}

// AuditStatus reports the capture subsystem, including the durable-storage
// prerequisite, so a caller learns before starting a session that capture could
// never work.
func (s *Service) auditStatus(ctx context.Context, _ AuditStatusInput) (*AuditStatusOutput, error) {
	status, err := s.client.debug(ctx)
	if err != nil {
		return nil, err
	}
	out := &AuditStatusOutput{
		Enabled:        status.Enabled,
		KnownClients:   list(status.KnownClients),
		KnownProviders: list(status.KnownProviders),
		KnownModels:    list(status.KnownModels),
		Until:          status.Until,
		TTL:            status.TTL,
		MaxBytes:       status.MaxBytes,
		Warning:        status.Warning,
		Retention:      retentionNote,
		Sessions:       make([]AuditSession, 0, len(status.Sessions)),
	}
	for _, session := range status.Sessions {
		out.Sessions = append(out.Sessions, AuditSession{
			ID: session.ID, Clients: session.Clients, Providers: session.Providers,
			Models: session.Models, Duration: session.Duration, Until: session.Until,
			RanMs: session.RanMs, Captures: session.Captures,
		})
	}
	storage, err := s.client.storageSignal(ctx)
	if err != nil {
		// The session state is the answer; a missing storage signal is a note,
		// not a failure that hides it.
		out.Warning = strings.TrimSpace(out.Warning + " storage signal unavailable: " + err.Error())
		return out, nil
	}
	out.Storage = storage
	return out, nil
}

// AuditStartInput starts or edits one capture session.
type AuditStartInput struct {
	// SessionID edits that existing session in place. Omitted scope and
	// duration are PRESERVED for the edited session.
	SessionID string   `json:"session_id,omitempty" jsonschema:"edit this existing session instead of creating a new one"`
	Clients   []string `json:"clients,omitempty" jsonschema:"client scope; names within one dimension are alternatives"`
	Providers []string `json:"providers,omitempty" jsonschema:"provider scope"`
	Models    []string `json:"models,omitempty" jsonschema:"model scope; native model normalization applies"`
	// Duration is 15m, 1h, 6h, 12h, 24h, or empty for until stopped.
	Duration string `json:"duration,omitempty" jsonschema:"15m, 1h, 6h, 12h, 24h, or empty for until stopped"`
	// Confirm names the intended effect so an accidental call is visible in the
	// transcript.
	Confirm string `json:"confirm" jsonschema:"must be 'start capture': capture stores request and response BODIES and is sensitive"`
}

// AuditStartOutput is the resulting session state.
type AuditStartOutput struct {
	Started   AuditSession   `json:"started" jsonschema:"the session now in force"`
	Edited    bool           `json:"edited" jsonschema:"true when an existing session was edited in place"`
	Storage   StorageInfo    `json:"storage" jsonschema:"durable storage state at the time of the call"`
	Retention string         `json:"retention" jsonschema:"what this session's captures will outlive"`
	Sessions  []AuditSession `json:"sessions" jsonschema:"all live sessions after the change"`
	Warning   string         `json:"warning,omitempty" jsonschema:"the proxy applied the state in memory but could not persist it"`
}

// auditConfirmStart is the literal the caller must echo. Capture retains bodies,
// so the effect is never implicit.
const auditConfirmStart = "start capture"

// AuditStart starts or edits one capture session. It refuses when durable
// storage is off: the proxy would accept the session, report success, and never
// store a document, so a silent no-op is worse than a refusal.
func (s *Service) auditStart(ctx context.Context, in AuditStartInput) (*AuditStartOutput, error) {
	if in.Confirm != auditConfirmStart {
		return nil, fmt.Errorf("confirm must be %q: capture stores full request and response bodies and is sensitive", auditConfirmStart)
	}
	if in.SessionID == "" && len(in.Clients) == 0 && len(in.Providers) == 0 && len(in.Models) == 0 {
		return nil, fmt.Errorf("at least one of clients, providers or models is required; a capture session has no unrestricted all-traffic scope")
	}
	if err := validateCaptureDuration(in.Duration); err != nil {
		return nil, err
	}
	storage, err := s.client.storageSignal(ctx)
	if err != nil {
		return nil, err
	}
	if !storage.Enabled {
		return nil, fmt.Errorf("durable storage is disabled on this proxy (db_path is empty), so nothing could ever be captured: " +
			"a session would start, report success, and store no documents. Start the proxy with a db_path first")
	}
	body := map[string]any{"enabled": true}
	if in.SessionID != "" {
		body["id"] = in.SessionID
	}
	// Only send a dimension that was actually named: an omitted scope is
	// preserved on an edit and rejected on a create, whereas an explicit empty
	// array is a different document.
	if in.Clients != nil {
		body["clients"] = in.Clients
	}
	if in.Providers != nil {
		body["providers"] = in.Providers
	}
	if in.Models != nil {
		body["models"] = in.Models
	}
	if in.Duration != "" {
		body["duration"] = in.Duration
	}
	status, err := s.client.postDebug(ctx, body)
	if err != nil {
		return nil, err
	}
	return auditStateOutput(status, in.SessionID != "", storage, "capture documents stay readable until debug_capture_ttl expires; "+retentionNote), nil
}

// AuditStopInput stops one session, or all of them.
type AuditStopInput struct {
	// SessionID stops exactly that session.
	SessionID string `json:"session_id,omitempty" jsonschema:"stop exactly this session"`
	// StopAll requires the explicit StopAll confirmation token below, so a
	// session id left out by accident cannot clear every session.
	StopAll string `json:"stop_all,omitempty" jsonschema:"set to 'stop every capture session' to stop all of them at once"`
}

// AuditStopOutput is the state after stopping.
type AuditStopOutput struct {
	Stopped   string         `json:"stopped" jsonschema:"the session id that was stopped, or 'all'"`
	Sessions  []AuditSession `json:"sessions" jsonschema:"live sessions remaining"`
	Retention string         `json:"retention" jsonschema:"what stopping does NOT delete"`
	Warning   string         `json:"warning,omitempty" jsonschema:"the proxy applied the state in memory but could not persist it"`
}

// auditConfirmStopAll is the literal required to clear every session.
const auditConfirmStopAll = "stop every capture session"

// AuditStop stops one session, or every session when StopAll carries the exact
// confirmation token. Stopping never deletes stored captures.
func (s *Service) auditStop(ctx context.Context, in AuditStopInput) (*AuditStopOutput, error) {
	switch {
	case in.StopAll != "" && in.SessionID != "":
		return nil, fmt.Errorf("send either session_id or stop_all, not both")
	case in.StopAll != "":
		if in.StopAll != auditConfirmStopAll {
			return nil, fmt.Errorf("stop_all must be %q", auditConfirmStopAll)
		}
	case in.SessionID == "":
		return nil, fmt.Errorf("session_id is required to stop one session, or stop_all must be %q to stop every session", auditConfirmStopAll)
	}
	body := map[string]any{"enabled": false}
	stopped := "all"
	if in.SessionID != "" {
		body["id"] = in.SessionID
		stopped = in.SessionID
	}
	status, err := s.client.postDebug(ctx, body)
	if err != nil {
		return nil, err
	}
	out := &AuditStopOutput{Stopped: stopped, Retention: retentionNote, Warning: status.Warning}
	out.Sessions = auditSessions(status.Sessions)
	return out, nil
}

// AuditCapturesListInput pages captured request records.
type AuditCapturesListInput struct {
	// SessionID narrows the listing to one session.
	SessionID string `json:"session_id,omitempty" jsonschema:"list only captures from this session"`
	BeforeMs  int64  `json:"before_ms,omitempty" jsonschema:"cursor timestamp in unix milliseconds; send with before_id"`
	BeforeID  string `json:"before_id,omitempty" jsonschema:"cursor record id; send with before_ms"`
	Limit     int    `json:"limit,omitempty" jsonschema:"page size; smaller than this server's default is usually better"`
}

// AuditCapturesListOutput is one page of captured records.
type AuditCapturesListOutput struct {
	Records      []map[string]any `json:"records" jsonschema:"captured requests, newest first, with the record id to pass to audit_capture_get"`
	Returned     int              `json:"returned" jsonschema:"records in this page"`
	More         bool             `json:"more" jsonschema:"older rows are believed to exist"`
	Exhausted    bool             `json:"exhausted" jsonschema:"true when the cursor cannot advance; stop paging"`
	NextBeforeMs int64            `json:"next_before_ms,omitempty" jsonschema:"pass both cursor fields for the next older page"`
	NextBeforeID string           `json:"next_before_id,omitempty" jsonschema:"pass both cursor fields for the next older page"`
	Storage      StorageInfo      `json:"storage" jsonschema:"durable storage signal"`
	Truncation   Truncation       `json:"truncation" jsonschema:"whether records were withheld"`
	Note         string           `json:"note" jsonschema:"how this listing is produced and what it does not show"`
}

// auditListNote states the mechanism so the model does not assume a listing
// endpoint exists.
const auditListNote = "the proxy has no capture listing endpoint: this list is a bounded SELECT over requests WHERE debug = 1, " +
	"so it shows records whose capture document is stored, and nothing that expired or was never captured"

// AuditCapturesList pages captured requests through a bounded SQL probe.
func (s *Service) auditCapturesList(ctx context.Context, in AuditCapturesListInput) (*AuditCapturesListOutput, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = s.limits.PageSize
	}
	if limit > logPageMax {
		return nil, fmt.Errorf("limit must be at most %d", logPageMax)
	}
	if (in.BeforeMs == 0) != (in.BeforeID == "") {
		return nil, fmt.Errorf("before_ms and before_id must be sent together, exactly once each")
	}
	storage, err := s.client.storageSignal(ctx)
	if err != nil {
		return nil, err
	}
	if !storage.Enabled {
		return nil, fmt.Errorf("durable storage is disabled on this proxy (db_path is empty): no capture document can exist")
	}
	found, err := s.client.captureRecords(ctx, in.SessionID, in.BeforeMs, in.BeforeID, limit)
	if err != nil {
		return nil, err
	}
	kept, truncation := clamp(found, limit, "records")
	more := len(kept) == limit
	nextMs, nextID := captureCursor(kept)
	return &AuditCapturesListOutput{
		Records:      kept,
		Returned:     len(kept),
		More:         more,
		Exhausted:    exhausted(more, cursorAdvanced(in.BeforeMs, in.BeforeID, nextMs, nextID), len(kept)),
		NextBeforeMs: nextMs,
		NextBeforeID: nextID,
		Storage:      storage,
		Truncation:   truncation,
		Note:         auditListNote,
	}, nil
}

// captureCursor returns the keyset position the next older page starts at: the
// page is newest-first, so that is its LAST (oldest) row.
func captureCursor(page rows) (int64, string) {
	if len(page) == 0 {
		return 0, ""
	}
	oldest := page[len(page)-1]
	startedAt := int64(0)
	switch value := oldest["started_at"].(type) {
	case float64:
		startedAt = int64(value)
	case int64:
		startedAt = value
	}
	return startedAt, rowString(oldest, "id")
}

// AuditCaptureGetInput reads one stored capture document.
type AuditCaptureGetInput struct {
	// RecordID is the REQUEST RECORD id from audit_captures_list, not the
	// session id.
	RecordID string `json:"record_id" jsonschema:"the request RECORD id (requests.id), not the capture session id"`
	// MaxBytes is the document size above which the capture is withheld rather
	// than returned; the proxy's own capture budget still applies, and an absent
	// or expired document is 404.
	MaxBytes int `json:"max_bytes,omitempty" jsonschema:"document size limit in bytes; a larger document is withheld with a marker, never truncated into invalid JSON"`
}

// AuditCaptureGetOutput is the capture document.
type AuditCaptureGetOutput struct {
	RecordID string `json:"record_id" jsonschema:"the record this document belongs to"`
	// Document is the stored capture decoded as JSON: request and response
	// headers and bodies, timing, outcome, usage, cost and absorbed attempts.
	Document any `json:"document" jsonschema:"the stored capture document, or null when it was withheld for size"`
	// StoredBytes is the document's real size, so a withheld document is still
	// measurable.
	StoredBytes int        `json:"stored_bytes" jsonschema:"size of the stored document in bytes"`
	Truncation  Truncation `json:"truncation" jsonschema:"whether the document was withheld for size"`
	Sensitive   string     `json:"sensitive" jsonschema:"standing warning about this content"`
}

// captureSensitivity is the standing warning attached to every capture payload.
const captureSensitivity = "this document contains request and response BODIES. Credentials are redacted from headers, " +
	"but body content is not sanitized: treat everything here as sensitive and never paste it into an unrelated system"

// AuditCaptureGet returns one stored capture document by request record id. The
// document is sized before it is decoded, so an oversized capture is withheld
// with an explicit marker instead of being returned as a broken fragment.
func (s *Service) auditCaptureGet(ctx context.Context, in AuditCaptureGetInput) (*AuditCaptureGetOutput, error) {
	if strings.TrimSpace(in.RecordID) == "" {
		return nil, fmt.Errorf("record_id is required: it is a request record id from audit_captures_list, not a session id")
	}
	values := url.Values{}
	values.Set("id", in.RecordID)
	raw, err := s.client.getRaw(ctx, http.MethodGet, routeDebugCapture, values)
	if err != nil {
		return nil, err
	}
	out := &AuditCaptureGetOutput{RecordID: in.RecordID, StoredBytes: len(raw), Sensitive: captureSensitivity}
	limit := in.MaxBytes
	if limit <= 0 {
		limit = s.limits.CaptureBytes
	}
	if len(raw) > limit {
		out.Truncation = Truncation{
			Shown: 0, Total: len(raw), Truncated: true,
			Marker: fmt.Sprintf("withheld: the document is %d bytes, above the %d byte limit; raise max_bytes to read it",
				len(raw), limit),
		}
		return out, nil
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("millivolt returned HTTP 200 with a capture document that is not JSON (%d bytes): %v", len(raw), err)
	}
	out.Document = document
	return out, nil
}

// postDebug applies one debug session mutation and returns the resulting state.
// It is the single owner of the POST /admin/debug body and the response shape.
func (c *Client) postDebug(ctx context.Context, body map[string]any) (*debugStatus, error) {
	var status debugStatus
	if err := c.postJSON(ctx, routeDebug, body, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// storageSignal reads durable-storage availability from the bootstrap payload.
func (c *Client) storageSignal(ctx context.Context) (StorageInfo, error) {
	snapshot, err := c.bootstrap(ctx)
	if err != nil {
		return StorageInfo{}, err
	}
	return StorageInfo{
		Enabled:        snapshot.Storage.Enabled,
		Dropped:        snapshot.Storage.Dropped,
		TotalsDegraded: snapshot.Storage.TotalsDegraded,
	}, nil
}

func auditSessions(sessions []debugSession) []AuditSession {
	out := make([]AuditSession, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, AuditSession{
			ID: session.ID, Clients: session.Clients, Providers: session.Providers,
			Models: session.Models, Duration: session.Duration, Until: session.Until,
			RanMs: session.RanMs, Captures: session.Captures,
		})
	}
	return out
}

func auditStateOutput(status *debugStatus, edited bool, storage StorageInfo, retention string) *AuditStartOutput {
	return &AuditStartOutput{
		Started:   AuditSession{ID: firstSessionID(status.Sessions)},
		Edited:    edited,
		Storage:   storage,
		Retention: retention,
		Sessions:  auditSessions(status.Sessions),
		Warning:   status.Warning,
	}
}

func firstSessionID(sessions []debugSession) string {
	if len(sessions) == 0 {
		return ""
	}
	return sessions[len(sessions)-1].ID
}

// validateCaptureDuration applies the proxy's session-duration allowlist so a
// typo is a clear message instead of a 400.
func validateCaptureDuration(duration string) error {
	if duration == "" {
		return nil
	}
	switch duration {
	case "15m", "1h", "6h", "12h", "24h":
		return nil
	}
	return fmt.Errorf("duration must be 15m, 1h, 6h, 12h, 24h, or empty for until stopped")
}
