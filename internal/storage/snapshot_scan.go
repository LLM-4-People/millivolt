package storage

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// recordScanValueKind is the SQLite storage-class family one recordScanTargets
// destination accepts. It is derived from the destination Go type, never from
// a second hand-maintained column list.
type recordScanValueKind int

const (
	recordScanText recordScanValueKind = iota
	recordScanInteger
	recordScanReal
	recordScanBool
)

// recordScanValueKindOf maps one scan destination pointer to the value class
// its Go type accepts, plus the type's bit width for range checks. An unknown
// destination fails closed: a new scan type must teach the preflight its
// accepted representation instead of silently escaping validation.
func recordScanValueKindOf(target any) (recordScanValueKind, int, error) {
	t := reflect.TypeOf(target)
	if t == nil || t.Kind() != reflect.Pointer {
		return 0, 0, fmt.Errorf("scan destination %T is not a pointer", target)
	}
	switch t.Elem().Kind() {
	case reflect.String:
		return recordScanText, 0, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return recordScanInteger, t.Elem().Bits(), nil
	case reflect.Float32, reflect.Float64:
		return recordScanReal, t.Elem().Bits(), nil
	case reflect.Bool:
		return recordScanBool, 0, nil
	default:
		return 0, 0, fmt.Errorf("unsupported scan destination type %s", t.Elem())
	}
}

// recordScanCheck is one scan-critical column's rejection predicate and the
// operator-facing description of the values scanRecord accepts.
type recordScanCheck struct {
	column   string
	reject   string
	accepted string
}

// recordScanMillisMin and recordScanMillisMax bound every stored millisecond
// timestamp that scanRecord turns into a time.Time. time.Time.MarshalJSON
// rejects years outside [0,9999], so a value outside this range is accepted by
// SQLite and every later dashboard encode fails. The bounds are the first and
// last millisecond of that representable window.
const (
	recordScanMillisMin = int64(-62167219200000) // 0000-01-01T00:00:00Z
	recordScanMillisMax = int64(253402300799999) // 9999-12-31T23:59:59.999Z
	// recordScanDurationMaxMs is the largest duration_ms whose conversion to
	// time.Duration in scanRecord cannot overflow: duration_ms * time.Millisecond
	// must fit in int64 nanoseconds.
	recordScanDurationMaxMs = int64(math.MaxInt64) / int64(time.Millisecond)
)

// recordScanMillisColumns are the columns scanRecord converts through
// time.UnixMilli. Every field serialized as time.Time must be listed here.
var recordScanMillisColumns = []string{
	"started_at", "first_token_at", "last_token_at", "first_answer_at", "final_attempt_at",
}

// aggregateMetricColumns are the REAL columns whose values feed the checked
// aggregate domain (metrics.SumValues): the KPI cost total and the throughput
// sums. A finite but negative, or non-finite, value here poisons every
// aggregate and JSON encode, so restore admission applies the same domain the
// writer's accounting applies.
var aggregateMetricColumns = []string{"cost", "gen_tps"}

// aggregateCountColumns are the INTEGER request fields Totals.Add folds with
// metrics.SumCounts. A negative row would make that checked fold fail. The
// full int64 aggregate bound is owned separately by the staged/merged totals
// scan, not by a per-column SQL SUM.
var aggregateCountColumns = []string{
	"input_tokens", "output_tokens", "cache_read_tokens", "reasoning_tokens", "ttft_ms",
}

// recordScanChecks derives one value check per recordColumns entry from the
// scan destinations scanRecord itself uses. Every numeric destination accepts
// the schema's own storage classes only: INTEGER for an integer or bool
// destination, INTEGER or REAL for a real destination. Text and blob coercions
// are deliberately rejected even where database/sql would parse them, because
// the writer never produces them and accepting them would hide a poisoned
// snapshot. A text destination accepts the TEXT storage class only: SQLite
// permits a BLOB in a TEXT-affinity column, and the log cursor binds exact
// filter keys as TEXT, so a BLOB id scans as a Go string but can never be
// materialized by LogPageRecords.
func recordScanChecks() ([]recordScanCheck, error) {
	columns := recordScanColumns()
	var record metrics.Record
	var scratch recordScanScratch
	targets := recordScanTargets(&record, &scratch)
	if len(targets) != len(columns) {
		return nil, fmt.Errorf("scan destination count %d does not match record column count %d", len(targets), len(columns))
	}
	checks := make([]recordScanCheck, len(columns))
	for i, column := range columns {
		kind, bits, err := recordScanValueKindOf(targets[i])
		if err != nil {
			return nil, fmt.Errorf("record column %s: %w", column, err)
		}
		quoted := `"` + column + `"`
		check := recordScanCheck{column: column, reject: quoted + ` IS NULL`}
		switch kind {
		case recordScanText:
			check.accepted = "a text value"
			check.reject += ` OR typeof(` + quoted + `) <> 'text'`
		case recordScanInteger:
			check.accepted = "an integer"
			check.reject += ` OR typeof(` + quoted + `) <> 'integer'`
			if bits < 64 {
				minValue := -(int64(1) << (bits - 1))
				maxValue := int64(1)<<(bits-1) - 1
				check.reject += fmt.Sprintf(` OR %s < %d OR %s > %d`, quoted, minValue, quoted, maxValue)
			}
			if slices.Contains(recordScanMillisColumns, column) {
				check.accepted = "an integer millisecond timestamp within the RFC 3339 range"
				check.reject += fmt.Sprintf(` OR %s < %d OR %s > %d`,
					quoted, recordScanMillisMin, quoted, recordScanMillisMax)
			}
			if slices.Contains(aggregateCountColumns, column) {
				check.accepted = "a nonnegative integer"
				check.reject += ` OR ` + quoted + ` < 0`
			}
		case recordScanReal:
			check.accepted = "a number"
			check.reject += ` OR typeof(` + quoted + `) NOT IN ('integer', 'real')`
			if bits < 64 {
				// ParseFloat with the destination width rejects values beyond
				// the type's finite range; spell the bound exactly so the SQL
				// comparison matches the conversion.
				maxValue := strconv.FormatFloat(float64(math.MaxFloat32), 'g', -1, 64)
				check.reject += fmt.Sprintf(` OR %s > %s OR %s < -%s`, quoted, maxValue, quoted, maxValue)
			}
			if slices.Contains(aggregateMetricColumns, column) {
				// SQLite stores +Inf/-Inf as REAL and NaN as NULL. The generic
				// NULL and storage-class checks above already catch NaN; the
				// finite/nonnegative border below rejects the infinite and
				// negative per-row values metrics.SumValues refuses. The staged
				// totals check in preflightPendingSnapshot owns the aggregate
				// result through the real Totals.Add semantics.
				check.accepted = "a finite nonnegative number"
				maxValue := strconv.FormatFloat(math.MaxFloat64, 'g', -1, 64)
				check.reject += fmt.Sprintf(` OR %s < 0 OR %s > %s OR %s < -%s`,
					quoted, quoted, maxValue, quoted, maxValue)
			}
		case recordScanBool:
			check.accepted = "an integer 0 or 1"
			check.reject += ` OR typeof(` + quoted + `) <> 'integer' OR ` + quoted + ` NOT IN (0, 1)`
		default:
			return nil, fmt.Errorf("record column %s: unsupported scan value kind", column)
		}
		checks[i] = check
	}
	return checks, nil
}

// preflightRequestScanValues rejects a request table whose rows contain a
// value scanRecord cannot scan into its destination: NULL, a non-numeric
// storage class in a numeric or bool column, an out-of-range integer, or a
// bool other than 0/1. id is the classic NULL case because
// SQLite permits NULL in a legacy TEXT PRIMARY KEY, but stored values matter
// just as much: stream='abc' or stream=2 passes integrity, schema and writer
// probes and then makes every history read fail after a merge commits or a
// replacement is installed. The check runs before a merge transaction starts
// or a replacement is installed, so a rejected source leaves live history
// untouched. Empty strings, empty JSON columns, 0/1 booleans, and ordinary
// integers and reals remain valid.
func preflightRequestScanValues(ctx context.Context, db mergeConn, schema, source string) error {
	if schema != "" && schema != "backup_merge" && schema != "backup_inspect" {
		return fmt.Errorf("unknown backup schema")
	}
	table := "requests"
	if schema != "" {
		table = schema + ".requests"
	}
	details, err := tableColumnDetails(ctx, db, schema, "requests")
	if err != nil {
		return err
	}
	present := make(map[string]struct{}, len(details))
	for _, col := range details {
		present[col.name] = struct{}{}
	}
	all, err := recordScanChecks()
	if err != nil {
		return err
	}
	checks := make([]recordScanCheck, 0, len(all))
	for _, check := range all {
		if _, ok := present[check.column]; ok {
			checks = append(checks, check)
		}
	}
	if len(checks) == 0 {
		return fmt.Errorf("%s requests has no scan-required columns", source)
	}
	conditions := make([]string, len(checks))
	for i, check := range checks {
		conditions[i] = check.reject
	}
	var invalid int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+table+` WHERE `+strings.Join(conditions, " OR ")).Scan(&invalid); err != nil {
		return err
	}
	if invalid == 0 {
		if err := preflightRequestDuration(ctx, db, table, source, present); err != nil {
			return err
		}
		if _, ok := present["attempts"]; ok {
			var malformed int64
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+
				` WHERE attempts IS NOT NULL AND attempts <> '' AND json_valid(attempts) = 0`).Scan(&malformed); err != nil {
				return fmt.Errorf("%s requests.attempts: %w", source, err)
			}
			if malformed != 0 {
				return fmt.Errorf("%s requests.attempts contains %d malformed JSON values", source, malformed)
			}
		}
		return nil
	}
	// Only a failing table pays the per-column scans; the error names the first
	// scan-critical column that is actually invalid so an operator can inspect
	// the offending snapshot directly.
	for _, check := range checks {
		var count int64
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE `+check.reject).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			continue
		}
		var nulls int64
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE "`+check.column+`" IS NULL`).Scan(&nulls); err != nil {
			return err
		}
		if nulls != 0 {
			return fmt.Errorf("%s requests contains %d rows with a NULL %s; history rows require non-NULL values for every scanRecord column", source, nulls, check.column)
		}
		return fmt.Errorf("%s requests contains %d rows with an invalid %s value; scanRecord requires %s", source, count, check.column, check.accepted)
	}
	return fmt.Errorf("%s requests contains values scanRecord cannot read", source)
}

// preflightRequestDuration applies the derived-End domain shared by replace and
// merge. scanRecord multiplies duration_ms by time.Millisecond and adds it to
// started_at, so a negative or too-large duration either wraps the Duration or
// moves End outside the RFC 3339 window even when the multiplication is safe.
// The subtraction form keeps started_at + duration_ms from overflowing SQLite
// integers; the standalone bounds already reject values that would make the
// subtraction itself unsafe.
func preflightRequestDuration(ctx context.Context, db mergeConn, table, source string, present map[string]struct{}) error {
	if _, ok := present["duration_ms"]; !ok {
		return nil
	}
	maxDuration := strconv.FormatInt(recordScanDurationMaxMs, 10)
	invalid := `typeof("duration_ms") <> 'integer' OR "duration_ms" < 0 OR "duration_ms" > ` + maxDuration
	if _, ok := present["started_at"]; ok {
		invalid += ` OR "started_at" < ` + strconv.FormatInt(recordScanMillisMin, 10) + ` - "duration_ms"` +
			` OR "started_at" > ` + strconv.FormatInt(recordScanMillisMax, 10) + ` - "duration_ms"`
	}
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE `+invalid).Scan(&count); err != nil {
		return fmt.Errorf("%s requests.duration_ms: %w", source, err)
	}
	if count != 0 {
		return fmt.Errorf("%s requests contains %d rows with an invalid duration_ms value; duration must be nonnegative, representable as time.Duration, and keep End within the RFC 3339 range", source, count)
	}
	return nil
}
