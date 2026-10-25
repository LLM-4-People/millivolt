package storage

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const (
	retentionStoragePathMax         = 1024
	retentionStorageMethodMax       = 32
	retentionStorageProviderTextMax = 512
	retentionStorageToolNameMax     = 128
	retentionStorageToolNamesMax    = 64
	retentionStorageDebugSessionMax = 128
	retentionStorageMarker          = "…"
)

func assertStoredRecordBounded(t *testing.T, label string, got string, max int) {
	t.Helper()
	if len(got) > max {
		t.Fatalf("%s retained %d bytes, want <= %d", label, len(got), max)
	}
	if !strings.HasSuffix(got, retentionStorageMarker) {
		t.Fatalf("%s of %d bytes does not end with the retention marker", label, len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("%s of %d bytes is not valid UTF-8", label, len(got))
	}
}

func assertRecordRetention(t *testing.T, label string, rec *metrics.Record) {
	t.Helper()
	assertStoredRecordBounded(t, label+" path", rec.Path, retentionStoragePathMax)
	assertStoredRecordBounded(t, label+" method", rec.Method, retentionStorageMethodMax)
	assertStoredRecordBounded(t, label+" model", rec.Model, retentionStorageProviderTextMax)
	assertStoredRecordBounded(t, label+" provider model", rec.ProviderModel, retentionStorageProviderTextMax)
	assertStoredRecordBounded(t, label+" provider request id", rec.ProviderRequestID, retentionStorageProviderTextMax)
	assertStoredRecordBounded(t, label+" provider server", rec.ProviderServer, retentionStorageProviderTextMax)
	assertStoredRecordBounded(t, label+" finish reason", rec.FinishReason, retentionStorageProviderTextMax)
	assertStoredRecordBounded(t, label+" error type", rec.ErrorType, retentionStorageProviderTextMax)
	assertStoredRecordBounded(t, label+" error code", rec.ErrorCode, retentionStorageProviderTextMax)
	assertStoredRecordBounded(t, label+" debug session id", rec.DebugSessionID, retentionStorageDebugSessionMax)
	if len(rec.ToolNames) > retentionStorageToolNamesMax {
		t.Fatalf("%s retained %d tool names, want <= %d", label, len(rec.ToolNames), retentionStorageToolNamesMax)
	}
	for i, name := range rec.ToolNames {
		if len(name) > retentionStorageToolNameMax {
			t.Fatalf("%s tool name %d retained %d bytes, want <= %d", label, i, len(name), retentionStorageToolNameMax)
		}
	}
	if len(rec.Attempts) != 2 || rec.Attempts[0].StatusCode != 429 || rec.Attempts[1].StatusCode != 503 {
		t.Fatalf("%s attempts = %+v, want the original order", label, rec.Attempts)
	}
	for i := range rec.Attempts {
		a := &rec.Attempts[i]
		for field, got := range map[string]string{
			"provider model":      a.ProviderModel,
			"provider request id": a.ProviderRequestID,
			"provider server":     a.ProviderServer,
			"error type":          a.ErrorType,
			"error code":          a.ErrorCode,
		} {
			assertStoredRecordBounded(t, label+" attempt "+field, got, retentionStorageProviderTextMax)
		}
	}
	if rec.Attempts[0].ErrorMsg != "rate limited" || rec.Attempts[1].ErrorMsg != "provider unavailable" {
		t.Fatalf("%s attempt messages changed: %q / %q", label, rec.Attempts[0].ErrorMsg, rec.Attempts[1].ErrorMsg)
	}
}

// TestRecorderBoundsRecordRetentionInRingAndStore drives the shared
// ring+durable recorder with hostile 1 MiB provider/client metadata and proves
// both views and the raw SQL columns stay bounded. A normal record keeps its
// exact pre-entry JSON in the ring.
func TestRecorderBoundsRecordRetentionInRingAndStore(t *testing.T) {
	s, err := Open(testDatabase(t), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	b := metrics.NewBuffer(4)
	recorder := s.Recorder(b)

	normal := &metrics.Record{
		ID: "normal", Provider: "fixture", Model: "model-a", Path: "/v1/chat/completions",
		Method: "POST", FinishReason: "stop", ToolNames: []string{"read", "write"},
		StatusCode: 200, Start: time.Now(),
	}
	normalJSON, err := json.Marshal(normal)
	if err != nil {
		t.Fatal(err)
	}
	hostileRecord := hostileRetentionRecord("hostile")
	recorder.Record(normal)
	recorder.Record(hostileRecord)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	ring := b.Snapshot()
	if len(ring) != 2 {
		t.Fatalf("ring records = %d, want 2", len(ring))
	}
	normalRing, hostileRing := ring[0], ring[1]
	if normalRing.ID != "normal" || hostileRing.ID != "hostile" {
		t.Fatalf("ring IDs = %q/%q", normalRing.ID, hostileRing.ID)
	}
	normalAfter, err := json.Marshal(normalRing)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(normalJSON, normalAfter) {
		t.Fatalf("normal ring record changed:\nbefore=%s\nafter=%s", normalJSON, normalAfter)
	}
	assertRecordRetention(t, "ring", hostileRing)

	durable, err := s.LoadRecent(t.Context(), 10)
	if err != nil || len(durable) != 2 {
		t.Fatalf("durable records = %d err = %v, want 2", len(durable), err)
	}
	byID := make(map[string]*metrics.Record, len(durable))
	for _, rec := range durable {
		byID[rec.ID] = rec
	}
	if byID["normal"] == nil || byID["hostile"] == nil {
		t.Fatalf("durable IDs = %+v", byID)
	}
	normalDurable := byID["normal"]
	if normalDurable.Path != normal.Path || normalDurable.Method != normal.Method ||
		normalDurable.Model != normal.Model || normalDurable.FinishReason != normal.FinishReason ||
		len(normalDurable.ToolNames) != 2 || normalDurable.ToolNames[0] != "read" ||
		normalDurable.ToolNames[1] != "write" {
		t.Fatalf("normal durable record changed: %+v", normalDurable)
	}
	assertRecordRetention(t, "durable", byID["hostile"])

	// Pin the raw SQL side too: the enqueued value is the bounded record, so a
	// database column cannot retain the hostile input.
	var pathLen, methodLen, modelLen, providerModelLen, requestIDLen, serverLen, finishLen, errTypeLen, errCodeLen, debugLen int64
	if err := s.db.QueryRowContext(t.Context(), `SELECT
		LENGTH(CAST(path AS BLOB)), LENGTH(CAST(method AS BLOB)),
		LENGTH(CAST(model AS BLOB)), LENGTH(CAST(provider_model AS BLOB)),
		LENGTH(CAST(provider_request_id AS BLOB)), LENGTH(CAST(provider_server AS BLOB)),
		LENGTH(CAST(finish_reason AS BLOB)), LENGTH(CAST(error_type AS BLOB)),
		LENGTH(CAST(error_code AS BLOB)), LENGTH(CAST(debug_session_id AS BLOB))
		FROM requests WHERE id = ?`, "hostile").Scan(
		&pathLen, &methodLen, &modelLen, &providerModelLen, &requestIDLen,
		&serverLen, &finishLen, &errTypeLen, &errCodeLen, &debugLen); err != nil {
		t.Fatal(err)
	}
	for field, got := range map[string]struct {
		len int64
		max int
	}{
		"path":                {pathLen, retentionStoragePathMax},
		"method":              {methodLen, retentionStorageMethodMax},
		"model":               {modelLen, retentionStorageProviderTextMax},
		"provider_model":      {providerModelLen, retentionStorageProviderTextMax},
		"provider_request_id": {requestIDLen, retentionStorageProviderTextMax},
		"provider_server":     {serverLen, retentionStorageProviderTextMax},
		"finish_reason":       {finishLen, retentionStorageProviderTextMax},
		"error_type":          {errTypeLen, retentionStorageProviderTextMax},
		"error_code":          {errCodeLen, retentionStorageProviderTextMax},
		"debug_session_id":    {debugLen, retentionStorageDebugSessionMax},
	} {
		if got.len > int64(got.max) {
			t.Fatalf("durable %s column = %d bytes, want <= %d", field, got.len, got.max)
		}
	}
}

func hostileRetentionRecord(id string) *metrics.Record {
	hostile := strings.Repeat("é", 1<<19)
	toolNames := make([]string, retentionStorageToolNamesMax+1)
	for i := range toolNames {
		toolNames[i] = "tool"
	}
	toolNames[0] = hostile
	return &metrics.Record{
		ID: id, Provider: "fixture", Path: hostile, Method: hostile, Model: hostile,
		ProviderModel: hostile, ProviderRequestID: hostile, ProviderServer: hostile,
		FinishReason: hostile, ErrorType: hostile, ErrorCode: hostile,
		DebugSessionID: hostile, ToolNames: toolNames,
		Start: time.Now(), StatusCode: 503,
		Attempts: []metrics.RetryAttempt{
			{
				StatusCode: 429, ErrorType: hostile, ErrorCode: hostile, ErrorMsg: "rate limited",
				ProviderModel: hostile, ProviderRequestID: hostile, ProviderServer: hostile,
			},
			{
				StatusCode: 503, ErrorType: hostile, ErrorCode: hostile, ErrorMsg: "provider unavailable",
				ProviderModel: hostile, ProviderRequestID: hostile, ProviderServer: hostile,
			},
		},
	}
}

func retentionColumnValues(t *testing.T, s *Store, id string) map[string]string {
	t.Helper()
	var path, method, model, providerModel, requestID, server, finish, errorType, errorCode, debugSession, toolNames, attempts string
	err := s.rdb.QueryRow(`SELECT path, method, model, provider_model, provider_request_id,
		provider_server, finish_reason, error_type, error_code, debug_session_id,
		tool_names, attempts FROM requests WHERE id = ?`, id).Scan(
		&path, &method, &model, &providerModel, &requestID, &server,
		&finish, &errorType, &errorCode, &debugSession, &toolNames, &attempts)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"path": path, "method": method, "model": model,
		"provider_model": providerModel, "provider_request_id": requestID,
		"provider_server": server, "finish_reason": finish,
		"error_type": errorType, "error_code": errorCode,
		"debug_session_id": debugSession, "tool_names": toolNames, "attempts": attempts,
	}
}

// TestDirectStoreRecordAppliesSharedRetention pins the exported Store.Record
// contract: it is a valid metrics.Recorder without a ring, so the durable
// boundary itself must apply the same retention as Recorder(buffer). The two
// paths must persist identical bounded fields.
func TestDirectStoreRecordAppliesSharedRetention(t *testing.T) {
	direct, err := Open(testDatabase(t), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	production, err := Open(testDatabase(t), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer production.Close()

	direct.Record(hostileRetentionRecord("hostile"))
	production.Recorder(metrics.NewBuffer(4)).Record(hostileRetentionRecord("hostile"))
	if err := direct.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := production.Flush(); err != nil {
		t.Fatal(err)
	}

	directRows, err := direct.LoadRecent(t.Context(), 1)
	if err != nil || len(directRows) != 1 {
		t.Fatalf("direct rows = %d, %v", len(directRows), err)
	}
	assertRecordRetention(t, "direct", directRows[0])

	directColumns := retentionColumnValues(t, direct, "hostile")
	productionColumns := retentionColumnValues(t, production, "hostile")
	for column, want := range productionColumns {
		if got := directColumns[column]; got != want {
			t.Fatalf("direct %s differs from production recorder: got %d bytes, want %d bytes", column, len(got), len(want))
		}
	}
}
