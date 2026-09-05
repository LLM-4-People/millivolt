package web

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func rateLimitRecords() []*metrics.Record {
	records := []*metrics.Record{
		{ID: "final429", StatusCode: 429, ErrorType: "insufficient_quota"},
		{ID: "recovered429", StatusCode: 200, Attempts: []metrics.RetryAttempt{{StatusCode: 429}, {StatusCode: 429}}},
		{ID: "recovered503", StatusCode: 200, RateLimited: true, Attempts: []metrics.RetryAttempt{{StatusCode: 503}}},
		{ID: "both-final429", StatusCode: 429, Attempts: []metrics.RetryAttempt{{StatusCode: 500}, {StatusCode: 500}}},
		{ID: "both-final500", StatusCode: 500, Attempts: []metrics.RetryAttempt{{StatusCode: 429}}},
		{ID: "queue-only", StatusCode: 200, QueueWaitMs: 10, RateLimited: true},
		{ID: "clean", StatusCode: 200},
	}
	for i, record := range records {
		record.Start = time.Unix(1700000000+int64(i), 0)
		record.End = record.Start.Add(time.Second)
		record.Provider, record.Model, record.Client, record.KeyHash = "neutral.example", "model", "client", "key"
		record.ConversationID = fmt.Sprint("conversation-", i%2)
		record.ToolNames, record.ToolCalls = []string{"lookup", "lookup"}, 2
	}
	return records
}

func TestExplorerDistinctFailureRequest(t *testing.T) {
	record := &metrics.Record{ID: "one-request", Start: time.Unix(1700000000, 0), StatusCode: 200,
		Provider: "neutral.example", ToolCalls: 2, ToolNames: []string{"lookup", "lookup"},
		Attempts: []metrics.RetryAttempt{{StatusCode: 500}, {StatusCode: 500}}}
	row := fromRecord(record, nil)
	projection := explorerProjection([]contrib{row})
	for _, dim := range []string{"tool", "error"} {
		for _, encoded := range []bool{false, true} {
			t.Run(fmt.Sprint(dim, encoded), func(t *testing.T) {
				fold := newExplorerFold(dim, "", nil)
				input := &row
				if encoded {
					fold.prepare(projection, nil)
					input = &projection.rows[0]
				}
				if err := fold.fold(input); err != nil {
					t.Fatal(err)
				}
				result := fold.payload()
				if len(result.Groups) != 1 || result.Groups[0].ErrFinal != 1 {
					t.Fatalf("one affected error request must count once despite repeated membership: %+v", result.Groups)
				}
			})
		}
	}
}

func TestExplorerRateLimitRawProjectionParity(t *testing.T) {
	var rows []contrib
	for _, record := range rateLimitRecords() {
		rows = append(rows, fromRecord(record, nil))
	}
	projection := explorerProjection(rows)
	for _, dim := range dimensionNames {
		for _, filters := range [][]scopeFilter{nil, {{dim: "conversation", id: "conversation-0"}}, {{dim: "provider", id: "missing"}}, {{dim: "error", id: "http_500|500|"}}} {
			t.Run(fmt.Sprint(dim, filters), func(t *testing.T) {
				raw, projected := newExplorerFold(dim, "", filters), newExplorerFold(dim, "", filters)
				projected.prepare(projection, nil)
				for i := range rows {
					if err := raw.fold(&rows[i]); err != nil {
						t.Fatal(err)
					}
					if err := projected.fold(&projection.rows[i]); err != nil {
						t.Fatal(err)
					}
				}
				got, want := projected.payload(), raw.payload()
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("projected/raw mismatch:\n%+v\n%+v", got, want)
				}
				if len(filters) == 0 && (dim == "provider" || dim == "tool") {
					if len(got.Groups) != 1 || got.Groups[0].ErrFinal != 3 || got.Groups[0].RateLimitRequests != 4 {
						t.Fatalf("affected request counts must not count duplicate attempts/tool memberships: %+v", got.Groups)
					}
				}
			})
		}
	}
}

func TestExplorerHealthMembershipBoundaries(t *testing.T) {
	for _, dim := range []string{"tool", "error"} {
		fold := newExplorerFold(dim, "", nil)
		for i, id := range []string{"", "second-group", "third-request"} {
			name := "lookup"
			if i == 1 {
				name = "other"
			}
			row := contrib{id: id, start: int64(1000 + i), isErr: true, has429: true,
				toolsL: []string{name, name}, ent: []errEnt{{typ: name}, {typ: name}}}
			if err := fold.fold(&row); err != nil {
				t.Fatal(err)
			}
		}
		for _, group := range fold.payload().Groups {
			want := int64(2)
			if group.Name == "other" || group.Name == "other||" {
				want = 1
			}
			if group.ErrFinal != want || group.RateLimitRequests != want {
				t.Fatalf("%s empty ID / nonadjacent groups: %+v", dim, group)
			}
		}
	}
}

func TestExplorerRateLimitDurableRingPending(t *testing.T) {
	store := testStore(t)
	buffer := metrics.NewBuffer(64)
	api := NewAggAPI(buffer, store, time.Second)
	records := rateLimitRecords()
	for _, record := range records {
		store.Record(record)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	buffer.Backfill(records) // same finalized IDs must not count twice
	extra := &metrics.Record{ID: "ring-extra", Provider: "neutral.example", StatusCode: 429, Start: time.Now()}
	buffer.Record(extra)
	pending := &metrics.Record{ID: "pending", Provider: "neutral.example", Start: time.Now(), Attempts: []metrics.RetryAttempt{{StatusCode: 429}}}
	buffer.PublishLive("begin", pending)
	for _, status := range []string{"", "pending"} {
		payload, err := api.explorer(context.Background(), "provider", nil, status)
		if err != nil {
			t.Fatal(err)
		}
		want, failures := int64(6), int64(3)
		if status == "pending" {
			want, failures = 1, 0
		}
		if len(payload.Groups) != 1 || payload.Groups[0].RateLimitRequests != want || payload.Groups[0].ErrFinal != failures {
			t.Fatalf("status %q: durable+ring+pending counts = %+v", status, payload.Groups)
		}
	}
	pending.StatusCode = 429
	buffer.Record(pending)
	payload, err := api.explorer(context.Background(), "provider", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if payload.Groups[0].RateLimitRequests != 6 {
		t.Fatalf("finalized pending counted twice: %+v", payload.Groups)
	}
}
