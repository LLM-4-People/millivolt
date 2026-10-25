package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// ErrExportTimeout marks a log export that hit its own total budget. It is
// distinct from a caller cancellation or caller deadline so the HTTP layer and
// tests can tell "the server abandoned this export" from "the client went
// away". After the sentinel, the response may already contain a partial
// artifact; see ExportWhere.
var ErrExportTimeout = errors.New("log export exceeded its time budget")

// buildExportQuery is the one owner of the durable export statement. The
// filter predicate is the same parameterized predicate purge and count use.
// nowMs prunes expired debug captures while their request row is read.
func buildExportQuery(f PurgeFilter, nowMs int64) (string, []any) {
	// The page cursor and its debug capture ride one row from one pooled read
	// connection. Loading each capture through a second connection would make
	// enough concurrent exporters deadlock against the bounded read pool.
	q := `SELECT ` + recordColumns + `, CASE WHEN requests.debug = 0 THEN NULL ELSE (
		SELECT d.payload FROM request_debug AS d
		WHERE d.id = requests.id AND (d.expires_at = 0 OR d.expires_at > ?)) END AS debug_capture
		FROM requests`
	args := []any{nowMs}
	where, whereArgs := f.build() // the same parameterized predicate as purge/count
	if where != "" {
		q += ` WHERE ` + where
		args = append(args, whereArgs...)
	}
	q += ` ORDER BY started_at ASC, id ASC`
	return q, args
}

// exportFailure maps an export error to the caller's cancellation/deadline when
// that is what happened, otherwise to ErrExportTimeout when the internal export
// budget expired. A caller cancellation always wins over the internal deadline
// so the two outcomes stay distinguishable.
func exportFailure(parent, bounded context.Context, err error) error {
	if parentErr := parent.Err(); parentErr != nil {
		return parentErr
	}
	if errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrExportTimeout, context.DeadlineExceeded)
	}
	return err
}

// exportRowHook is a test seam (production leaves it nil). It runs after a row
// is fetched and before it is written, so a test can stall the row source under
// the export's own deadline without racing the SQLite driver.
var exportRowHook func(context.Context)

// ExportWhere streams every stored row matching the filter (an empty filter
// = all rows - export is read-only, so "everything" is safe) as a compact
// JSON array, one record per line, oldest-first, directly to the writer -
// memory stays bounded no matter how much history exists. Returns the number
// of records written.
//
// The whole stream is bounded by the internal exportTimeout (or the caller's
// shorter deadline), because an open export read transaction holds a WAL
// snapshot for its lifetime. A deadline that expires after the first byte was
// written leaves a partial, incomplete artifact on the caller's writer: the
// HTTP handler aborts the response instead of completing a 200 download. A
// caller cancellation or caller deadline is returned as that context error;
// the internal budget surfaces as ErrExportTimeout mixed with
// context.DeadlineExceeded.
func (s *Store) ExportWhere(ctx context.Context, f PurgeFilter, w io.Writer) (int64, error) {
	if err := f.Validate(); err != nil {
		return 0, err
	}
	bounded, cancel := s.withExportTimeout(ctx)
	defer cancel()
	q, args := buildExportQuery(f, time.Now().UnixMilli())
	rows, err := s.rdb.QueryContext(bounded, q, args...)
	if err != nil {
		return 0, exportFailure(ctx, bounded, err)
	}
	defer rows.Close()
	var n int64
	write := func(text string) error {
		if err := bounded.Err(); err != nil {
			return err
		}
		_, err := io.WriteString(w, text)
		return err
	}
	if err := write("[\n"); err != nil {
		return 0, exportFailure(ctx, bounded, err)
	}
	for rows.Next() {
		if exportRowHook != nil {
			exportRowHook(bounded)
		}
		if err := bounded.Err(); err != nil {
			return n, exportFailure(ctx, bounded, err)
		}
		r, capture, err := scanRecord(rows)
		if err != nil {
			return n, exportFailure(ctx, bounded, err)
		}
		b, err := marshalExport(r, capture)
		if err != nil {
			return n, exportFailure(ctx, bounded, err)
		}
		if n > 0 {
			if err := write(",\n"); err != nil {
				return n, exportFailure(ctx, bounded, err)
			}
		}
		if _, err := w.Write(b); err != nil {
			return n, exportFailure(ctx, bounded, err)
		}
		if err := bounded.Err(); err != nil {
			return n, exportFailure(ctx, bounded, err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, exportFailure(ctx, bounded, err)
	}
	if err := write("\n]\n"); err != nil {
		return n, exportFailure(ctx, bounded, err)
	}
	return n, nil
}

// marshalExport encodes one record for log download. Debug captures ride
// as a sibling "debug_capture" object so the ring JSON stays small. The
// capture is read with the record's own export row, never through a second
// read-pool acquisition.
func marshalExport(r *metrics.Record, capture []byte) ([]byte, error) {
	if r == nil {
		return []byte("null"), nil
	}
	if !r.Debug || len(capture) == 0 {
		return json.Marshal(r)
	}
	type exportRec struct {
		*metrics.Record
		DebugCapture json.RawMessage `json:"debug_capture,omitempty"`
	}
	return json.Marshal(exportRec{Record: r, DebugCapture: capture})
}
