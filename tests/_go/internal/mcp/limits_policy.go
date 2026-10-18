package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestMCPNegativeLimitsRejectedBeforeUpstream pins the one limit policy at
// every affected tool: negative is an invalid argument (not "unset"), and the
// rejection happens before any proxy request.
func TestMCPNegativeLimitsRejectedBeforeUpstream(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		field string
		call  func(*Service) error
	}{
		{"query max_rows", "max_rows", func(s *Service) error {
			_, err := s.query(ctx, QueryInput{SQL: "SELECT 1", MaxRows: -1})
			return err
		}},
		{"snapshot max_records", "max_records", func(s *Service) error {
			_, err := s.snapshot(ctx, SnapshotInput{MaxRecords: -1})
			return err
		}},
		{"values limit", "limit", func(s *Service) error {
			_, err := s.values(ctx, ValuesInput{Dim: "client", Limit: -1})
			return err
		}},
		{"audit_captures_list limit", "limit", func(s *Service) error {
			_, err := s.auditCapturesList(ctx, AuditCapturesListInput{Limit: -1})
			return err
		}},
		{"audit_capture_get max_bytes", "max_bytes", func(s *Service) error {
			_, err := s.auditCaptureGet(ctx, AuditCaptureGetInput{RecordID: "r1", MaxBytes: -1})
			return err
		}},
		{"records limit", "limit", func(s *Service) error {
			_, err := s.records(ctx, RecordsInput{Limit: -1})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := newFakeProxy(t)
			service := newTestService(t, proxy, Limits{})
			err := tc.call(service)
			if err == nil {
				t.Fatal("a negative limit must be refused")
			}
			if !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "-1") {
				t.Fatalf("error %q must name %q and the rejected value", err, tc.field)
			}
			if got := len(proxy.requests()); got != 0 {
				t.Fatalf("a refused limit reached the proxy (%d requests)", got)
			}
		})
	}
}

// TestResolveLimitPolicy pins the shared owner: zero selects the configured
// default, negative is rejected, positive is preserved inside the band, and an
// out-of-band positive is refused.
func TestResolveLimitPolicy(t *testing.T) {
	for _, tc := range []struct {
		field    string
		def      int
		min, max int
	}{
		{"max_rows", 50, 1, 100000},
		{"max_records", 50, 1, 0},
		{"limit", 50, 1, 500},
		{"max_bytes", 1024, 1, 0},
	} {
		t.Run(tc.field, func(t *testing.T) {
			got, err := resolveLimit(tc.field, 0, tc.def, tc.min, tc.max)
			if err != nil || got != tc.def {
				t.Fatalf("resolveLimit(0) = %d, %v, want the default %d", got, err, tc.def)
			}
			got, err = resolveLimit(tc.field, tc.min, tc.def, tc.min, tc.max)
			if err != nil || got != tc.min {
				t.Fatalf("resolveLimit(%d) = %d, %v, want the positive value", tc.min, got, err)
			}
			if _, err := resolveLimit(tc.field, -1, tc.def, tc.min, tc.max); err == nil {
				t.Fatal("a negative limit must be refused")
			}
			if tc.max > 0 {
				if _, err := resolveLimit(tc.field, tc.max+1, tc.def, tc.min, tc.max); err == nil {
					t.Fatal("an over-band limit must be refused")
				}
			}
		})
	}
}

// TestMCPZeroLimitUsesDefaultAndPositivePasses drives the handler wiring, not
// only the helper: zero keeps the default page, while an explicit positive
// limit is applied, across every affected tool.
func TestMCPZeroLimitUsesDefaultAndPositivePasses(t *testing.T) {
	const threeRows = `[{"id":"a"},{"id":"b"},{"id":"c"}]`
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, `{"seq":1,"feed_id":"f","counters":{},"storage":{"enabled":true},"records":[{"id":"a"},{"id":"b"},{"id":"c"}]}`)
	proxy.json(http.MethodGet, schemaPath, threeRows)
	proxy.json(http.MethodGet, capturePath, `{}`)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		call     func(*testing.T, int) error
		explicit int
	}{
		{"query max_rows", func(t *testing.T, limit int) error {
			out, err := service.query(ctx, QueryInput{SQL: "SELECT 1", MaxRows: limit})
			if err == nil {
				want := 3
				if limit > 0 {
					want = min(limit, want)
				}
				if out.RowCount != want {
					t.Errorf("query rows = %d for limit %d, want %d", out.RowCount, limit, want)
				}
			}
			return err
		}, 1},
		{"snapshot max_records", func(t *testing.T, limit int) error {
			out, err := service.snapshot(ctx, SnapshotInput{MaxRecords: limit})
			if err == nil {
				want := 3
				if limit > 0 && limit < want {
					want = limit
				}
				if len(out.Records) != want {
					t.Errorf("snapshot records = %d for limit %d, want %d", len(out.Records), limit, want)
				}
			}
			return err
		}, 1},
		{"values limit", func(t *testing.T, limit int) error {
			out, err := service.values(ctx, ValuesInput{Dim: "client", Limit: limit})
			if err == nil {
				want := 3
				if limit > 0 && limit < want {
					want = limit
				}
				if out.Returned != want {
					t.Errorf("values returned = %d for limit %d, want %d", out.Returned, limit, want)
				}
			}
			return err
		}, 1},
		{"audit_captures_list limit", func(t *testing.T, limit int) error {
			out, err := service.auditCapturesList(ctx, AuditCapturesListInput{Limit: limit})
			if err == nil {
				want := 3
				if limit > 0 && limit < want {
					want = limit
				}
				if out.Returned != want {
					t.Errorf("captures returned = %d for limit %d, want %d", out.Returned, limit, want)
				}
			}
			return err
		}, 1},
		{"audit_capture_get max_bytes", func(t *testing.T, limit int) error {
			out, err := service.auditCaptureGet(ctx, AuditCaptureGetInput{RecordID: "r1", MaxBytes: limit})
			if err == nil {
				if limit == 0 && out.Document == nil {
					t.Error("the default byte budget must return the document")
				}
				if limit > 0 && !out.Truncation.Truncated {
					t.Error("a one-byte budget must withhold the two-byte document")
				}
			}
			return err
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(t, 0); err != nil {
				t.Fatalf("zero/default limit: %v", err)
			}
			if err := tc.call(t, tc.explicit); err != nil {
				t.Fatalf("positive limit: %v", err)
			}
		})
	}
}
