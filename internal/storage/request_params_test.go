package storage

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestRequestParameterPresenceSurvivesRestartAndExport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request-params.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	zeroInt, zeroFloat, zeroSeed, no := 0, 0.0, int64(0), false
	zero := &metrics.Record{ID: "zero", Start: time.Now(), ReqMaxTokens: &zeroInt,
		ReqTemperature: &zeroFloat, ReqTopP: &zeroFloat, ReqN: &zeroInt,
		ReqPresencePen: &zeroFloat, ReqFrequencyPen: &zeroFloat,
		ReqSeed: &zeroSeed, ReqParallelTools: &no, ReqTopLogprobs: &zeroInt}
	absent := &metrics.Record{ID: "absent", Start: time.Now()}
	oneInt, oneFloat, oneSeed, yes := 1, 0.5, int64(42), true
	set := &metrics.Record{ID: "set", Start: time.Now(), ReqMaxTokens: &oneInt,
		ReqTemperature: &oneFloat, ReqTopP: &oneFloat, ReqN: &oneInt,
		ReqPresencePen: &oneFloat, ReqFrequencyPen: &oneFloat,
		ReqSeed: &oneSeed, ReqParallelTools: &yes, ReqTopLogprobs: &oneInt}
	for _, r := range []*metrics.Record{zero, absent, set} {
		s.Record(r)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := s.ExportWhere(t.Context(), PurgeFilter{}, &out); err != nil {
		t.Fatal(err)
	}
	var exported []map[string]any
	if err := json.Unmarshal(out.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	wantByID := map[string]*metrics.Record{"zero": zero, "absent": absent, "set": set}
	// Derive the request-param JSON keys from actual records rather than a
	// second field list; the canonical presence map is the storage owner.
	zeroJSON, _ := json.Marshal(zero)
	absentJSON, _ := json.Marshal(absent)
	var zeroMap, absentMap map[string]any
	json.Unmarshal(zeroJSON, &zeroMap)
	json.Unmarshal(absentJSON, &absentMap)
	var keys []string
	for key := range zeroMap {
		if _, exists := absentMap[key]; !exists {
			keys = append(keys, key)
		}
	}
	if len(keys) != len(requestParamFields) {
		t.Fatalf("fixture covers %d pointer fields, want %d", len(keys), len(requestParamFields))
	}
	for _, row := range exported {
		wantJSON, _ := json.Marshal(wantByID[row["id"].(string)])
		var want map[string]any
		json.Unmarshal(wantJSON, &want)
		for _, key := range keys {
			gotVal, gotOK := row[key]
			wantVal, wantOK := want[key]
			if gotOK != wantOK || !reflect.DeepEqual(gotVal, wantVal) {
				t.Errorf("%s %s: got %v/%v want %v/%v", row["id"], key, gotVal, gotOK, wantVal, wantOK)
			}
		}
	}
	rows, err := s.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if requestParamPresence(row) != requestParamPresence(wantByID[row.ID]) {
			t.Fatalf("reopened pointer mask lost for %s", row.ID)
		}
	}
}

func TestRequestParameterMigrationDoesNotInventLegacyZeros(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-params.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	one := 0.75
	s.Record(&metrics.Record{ID: "legacy", Start: time.Now(), ReqTemperature: &one})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	// Emulate an old schema, then let the real migration add presence metadata.
	if _, err := s.db.Exec("ALTER TABLE requests DROP COLUMN req_param_presence"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ReqTemperature == nil || *rows[0].ReqTemperature != one || rows[0].ReqSeed != nil || rows[0].ReqParallelTools != nil || rows[0].ReqMaxTokens != nil {
		t.Fatalf("legacy presence was invented or lost: %+v", rows)
	}
	var mask int64
	if err := s.rdb.QueryRow("SELECT req_param_presence FROM requests WHERE id='legacy'").Scan(&mask); err != nil || mask >= 0 {
		t.Fatalf("legacy marker=%d err=%v", mask, err)
	}
}

func TestRequestParameterPartialMaskPinsDurableBitOrder(t *testing.T) {
	s := openPurgeTestStore(t, testOpts)
	zero, no := 0.0, false
	s.Record(&metrics.Record{ID: "partial", Start: time.Now(), ReqTemperature: &zero, ReqParallelTools: &no})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	// A literal persisted fixture deliberately does not derive its bits from
	// requestParamFields: temperature is bit 1 and parallel_tools is bit 7.
	const storedMask = 130
	var mask int64
	if err := s.rdb.QueryRow("SELECT req_param_presence FROM requests WHERE id='partial'").Scan(&mask); err != nil || mask != storedMask {
		t.Fatalf("persisted mask=%d want %d err=%v", mask, storedMask, err)
	}
	if _, err := s.db.Exec("UPDATE requests SET req_param_presence=? WHERE id='partial'", storedMask); err != nil {
		t.Fatal(err)
	}
	rows, err := s.LoadRecent(t.Context(), 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("partial record: rows=%d err=%v", len(rows), err)
	}
	r := rows[0]
	if r.ReqTemperature == nil || *r.ReqTemperature != 0 || r.ReqParallelTools == nil || *r.ReqParallelTools {
		t.Fatalf("fixed mask lost explicit temperature/parallel_tools: %+v", r)
	}
	if r.ReqMaxTokens != nil || r.ReqTopP != nil || r.ReqN != nil || r.ReqPresencePen != nil || r.ReqFrequencyPen != nil || r.ReqSeed != nil || r.ReqTopLogprobs != nil {
		t.Fatalf("fixed mask invented absent request parameters: %+v", r)
	}
}
