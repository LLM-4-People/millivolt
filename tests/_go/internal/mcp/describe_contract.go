package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestDescribeCollectionsMarshalAsArrays pins the JSON contract: every array
// field is an array even on the debug-failure and storage-off paths, so a
// consumer can iterate without a null check. Length checks alone cannot catch
// a nil slice because len(nil) is zero.
func TestDescribeCollectionsMarshalAsArrays(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeProxy)
	}{
		{"debug failure", func(proxy *fakeProxy) {
			proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
			proxy.fail(http.MethodGet, debugPath, http.StatusInternalServerError, "capture state unavailable", "")
			proxy.json(http.MethodGet, schemaPath, `[]`)
		}},
		{"storage off", func(proxy *fakeProxy) {
			proxy.json(http.MethodGet, bootstrapPath, storageOffBootstrap)
			proxy.json(http.MethodGet, debugPath, emptyDebugStatus)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := newFakeProxy(t)
			tc.setup(proxy)
			service := newTestService(t, proxy, Limits{})
			out, err := service.describe(context.Background(), DescribeInput{})
			if err != nil {
				t.Fatalf("describe() = %v", err)
			}
			for _, slice := range []struct {
				name  string
				value []string
			}{
				{"known_clients", out.KnownClients},
				{"known_providers", out.KnownProviders},
				{"known_models", out.KnownModels},
				{"known_tools", out.KnownTools},
			} {
				if slice.value == nil {
					t.Errorf("%s must be an initialized slice", slice.name)
				}
			}
			if out.Schema == nil {
				t.Error("schema must be an initialized slice")
			}

			encoded, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := json.Unmarshal(encoded, &document); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"schema", "known_clients", "known_providers", "known_models", "known_tools"} {
				if _, ok := document[key].([]any); !ok {
					t.Errorf("%s = %#v, want a JSON array", key, document[key])
				}
			}
			vocabularies, ok := document["vocabularies"].([]any)
			if !ok {
				t.Fatalf("vocabularies = %#v, want a JSON array", document["vocabularies"])
			}
			for _, raw := range vocabularies {
				vocabulary, ok := raw.(map[string]any)
				if !ok {
					t.Fatalf("vocabulary = %#v, want an object", raw)
				}
				if _, ok := vocabulary["values"].([]any); !ok {
					t.Errorf("vocabulary %v values = %#v, want a JSON array", vocabulary["dimension"], vocabulary["values"])
				}
			}
		})
	}
}

// TestSnapshotAdviceFollowsStorage pins the storage branch: when durable
// history exists the snapshot points at the records cursor; with storage off
// the ring is the only history, so the advice names max_records instead of the
// empty records tool.
func TestSnapshotAdviceFollowsStorage(t *testing.T) {
	const enabled = `{"seq":2,"feed_id":"f","counters":{},"storage":{"enabled":true},"records":[{"id":"a"},{"id":"b"}]}`
	const disabled = `{"seq":2,"feed_id":"f","counters":{},"storage":{"enabled":false},"records":[{"id":"a"},{"id":"b"}]}`
	for _, tc := range []struct {
		name      string
		bootstrap string
		want      []string
		avoid     []string
	}{
		{
			name:      "storage enabled keeps the durable cursor advice",
			bootstrap: enabled,
			want:      []string{"records tool", "before_ms", "before_id"},
		},
		{
			name:      "storage disabled names the ring and max_records",
			bootstrap: disabled,
			want:      []string{"durable storage is disabled", "ring", "max_records"},
			avoid:     []string{"records tool", "before_ms", "before_id"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := newFakeProxy(t)
			proxy.json(http.MethodGet, bootstrapPath, tc.bootstrap)
			service := newTestService(t, proxy, Limits{})
			out, err := service.snapshot(context.Background(), SnapshotInput{MaxRecords: 1})
			if err != nil {
				t.Fatal(err)
			}
			if !out.Truncation.Truncated {
				t.Fatalf("snapshot truncation = %+v, want one record withheld", out.Truncation)
			}
			for _, text := range []string{out.OlderHistory, out.Truncation.Marker} {
				for _, needle := range tc.want {
					if !strings.Contains(text, needle) {
						t.Fatalf("snapshot advice %q must contain %q", text, needle)
					}
				}
				for _, needle := range tc.avoid {
					if strings.Contains(text, needle) {
						t.Fatalf("snapshot advice %q must not contain %q", text, needle)
					}
				}
			}
		})
	}
}
