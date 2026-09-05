package storage

import (
	"database/sql"
	"math"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// Compare every column against ordinary fully bound SQLite inserts. A sequence
// of one-column changes exercises every promotion; the final empty row ensures
// prior bindings cannot leak. The query/values lists come from the actual owner.
func TestBatchInsertMatchesFullBindings(t *testing.T) {
	open := func(name string) *Store {
		s, err := Open(filepath.Join(t.TempDir(), name+".db"), testOpts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	optimized, reference := open("optimized"), open("reference")
	columns := strings.Split(insertColumns[strings.IndexByte(insertColumns, '(')+1:len(insertColumns)-1], ",")
	types := map[string]string{}
	rows, err := reference.rdb.Query("SELECT name,type FROM pragma_table_info('requests')")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			t.Fatal(err)
		}
		types[name] = kind
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(columns) != len(types) {
		t.Fatalf("insert owner covers %d columns, schema has %d", len(columns), len(types))
	}
	zero, populated := make([]any, len(columns)), make([]any, len(columns))
	for i, column := range columns {
		switch types[strings.TrimSpace(column)] {
		case "TEXT":
			zero[i], populated[i] = "", "fixture '); -- "+column
		case "INTEGER":
			zero[i], populated[i] = int64(0), int64(i+1)
		case "REAL":
			zero[i], populated[i] = float64(0), float64(i)+0.125
		default:
			t.Fatalf("unhandled schema type for %s", column)
		}
	}
	var fixtures [][]any
	add := func(values []any) {
		copy := append([]any(nil), values...)
		copy[0] = "row-" + strconv.Itoa(len(fixtures))
		fixtures = append(fixtures, copy)
	}
	add(zero)
	add(zero)
	for i := 1; i < len(columns); i++ {
		row := append([]any(nil), zero...)
		row[i] = populated[i]
		add(row)
	}
	add(populated)
	add(zero)
	tx, err := optimized.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	plan := batchInsert{tx: tx}
	defer plan.close()
	full, err := reference.db.Prepare(insertColumns + " VALUES (" + strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",") + ")")
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	var first *sql.Stmt
	for i, row := range fixtures {
		if _, err := full.Exec(row...); err != nil {
			t.Fatal(err)
		}
		if err := plan.exec(append([]any(nil), row...)); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = plan.stmt
			for column, bound := range plan.bound {
				if bound != (column == 0) {
					t.Fatalf("initial bound column %d = %v; only ID should be bound", column, bound)
				}
			}
		} else if i == 1 && plan.stmt != first {
			t.Fatal("same request shape recompiled the statement")
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	load := func(s *Store) [][]any {
		rows, err := s.rdb.Query("SELECT " + strings.Join(columns, ",") + " FROM requests ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out [][]any
		for rows.Next() {
			row, ptrs := make([]any, len(columns)), make([]any, len(columns))
			for i := range row {
				ptrs[i] = &row[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out = append(out, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got, want := load(optimized), load(reference); !reflect.DeepEqual(got, want) {
		t.Fatal("adaptive inserts differ from fully bound inserts")
	}
}

func TestWriterRollbackAfterShapePromotion(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "rollback.db"), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first := &metrics.Record{ID: "first", Start: time.Now(), StatusCode: 200}
	bad := &metrics.Record{ID: "bad", Start: first.Start, StatusCode: 200, Cost: math.NaN()}
	if err := s.insertBatch([]*metrics.Record{first, bad}); err == nil {
		t.Fatal("nonrepresentable SQLite value must reject the batch")
	}
	var n int
	if err := s.rdb.QueryRow("SELECT count(*) FROM requests").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 || s.Totals().Requests != 0 || s.IsWritten(first.ID) || s.IsWritten(bad.ID) {
		t.Fatalf("failed batch partially committed: rows=%d totals=%+v", n, s.Totals())
	}
	// The failed statement/transaction must not poison subsequent batches.
	if err := s.insertBatch([]*metrics.Record{first}); err != nil {
		t.Fatal(err)
	}
	if s.Totals().Requests != 1 || !s.IsWritten(first.ID) {
		t.Fatal("successful replacement batch was not accounted exactly once")
	}
}

func TestInsertZeroOnlyElidesExactScalarZeros(t *testing.T) {
	for _, value := range []any{"", 0, int64(0), float64(0), math.Copysign(0, -1)} {
		if !insertZero(value) {
			t.Errorf("zero %T %v was not elided", value, value)
		}
	}
	for _, value := range []any{nil, false, []byte{}, "0", "null", " ", -1, int64(math.MaxInt64), math.SmallestNonzeroFloat64, math.NaN(), math.Inf(1)} {
		if insertZero(value) {
			t.Errorf("nonzero/unknown %T %v bypassed parameter binding", value, value)
		}
	}
}
