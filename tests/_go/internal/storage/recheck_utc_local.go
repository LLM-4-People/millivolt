//go:build !race

package storage

import (
	"encoding/json"
	"testing"
	"time"
)

// TestRestoreTimestampBoundariesMarshalInNonUTCZone pins the STO-2 residual:
// scanned times are UTC, so the admitted min/max boundaries still marshal in a
// process whose local zone would otherwise render year -1 or year 10000.
func TestRestoreTimestampBoundariesMarshalInNonUTCZone(t *testing.T) {
	for _, zone := range []struct {
		name   string
		offset int
	}{
		{"UTC-5", -5 * 60 * 60},
		{"UTC+14", 14 * 60 * 60},
	} {
		t.Run(zone.name, func(t *testing.T) {
			previous := time.Local
			time.Local = time.FixedZone(zone.name, zone.offset)
			defer func() { time.Local = previous }()

			for _, value := range []int64{recordScanMillisMin, recordScanMillisMax, time.Now().UnixMilli()} {
				data := snapshotWithValue(t, "started_at", value)
				live, err := replaceSnapshot(t, data)
				if err != nil {
					t.Fatalf("started_at %d rejected: %v", value, err)
				}
				rows, err := live.LoadRecent(t.Context(), 1)
				if err != nil {
					t.Fatalf("started_at %d load: %v", value, err)
				}
				if len(rows) != 1 {
					t.Fatalf("started_at %d rows = %d", value, len(rows))
				}
				if _, err := json.Marshal(rows[0]); err != nil {
					t.Fatalf("started_at %d does not marshal in %s: %v", value, zone.name, err)
				}
				if rows[0].Start.Location() != time.UTC || rows[0].End.Location() != time.UTC {
					t.Errorf("started_at %d locations = %v/%v, want UTC", value, rows[0].Start.Location(), rows[0].End.Location())
				}
			}
		})
	}
}
