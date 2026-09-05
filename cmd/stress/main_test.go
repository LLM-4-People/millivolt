package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestStressTargetSafety(t *testing.T) {
	o := options{levels: "1,8", duration: time.Second, timeout: time.Second, sample: time.Millisecond, chunks: 1, upstreams: 1, maxRSS: 128}
	for _, target := range []string{"", "http://127.0.0.1:8080", "http://example.com:8081", "http://0.0.0.0:8081", "http://127.0.0.1:8081/admin/restart", "http://user@127.0.0.1:8081", "http://127.0.0.1:8081?q=x"} {
		o.target = target
		if _, _, err := validate(o); err == nil {
			t.Errorf("unsafe target accepted: %s", target)
		}
	}
	o.target = "http://127.0.0.1:8081"
	if _, levels, err := validate(o); err != nil || len(levels) != 2 {
		t.Fatalf("valid dev workload rejected: %v", err)
	}
}

func TestStressSourceSafety(t *testing.T) {
	target := net.ParseIP("127.0.0.1")
	for _, value := range []string{"", "127.0.0.1,127.0.0.2"} {
		if _, err := sourceIPs(value, target); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"0.0.0.0", "example.com", "127.0.0.1,127.0.0.1", "::1", "127.0.0.1,"} {
		if _, err := sourceIPs(value, target); err == nil {
			t.Fatalf("unsafe sources accepted: %s", value)
		}
	}
}

func TestStressMetadataCannotIncludeProviderSecrets(t *testing.T) {
	got := performanceSettings(map[string]any{"providers": map[string]any{"headers": "secret"}, "max_conns_per_host": 512})
	if len(got) != 1 || got["max_conns_per_host"] != 512 {
		t.Fatal(got)
	}
}

func TestStressFirstFailureStopsNewWork(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	o := options{duration: time.Second, timeout: time.Second, sample: time.Millisecond, chunks: 1, maxRSS: 2048}
	client := localClient(o.timeout, &http.Transport{})
	defer client.CloseIdleConnections()
	res, err := stage(t.Context(), control{client, server.URL}, []*http.Client{client}, os.Getpid(), 100, o, []string{server.URL}, newFixture(o), 1, false)
	if err != nil || res.Requests != 1 || res.Failures != 1 || res.HTTPResponses != 1 || res.Successes != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestStressInitialRSSGuardStartsNoWork(t *testing.T) {
	o := options{maxRSS: .0001}
	res, err := stage(context.Background(), control{}, nil, os.Getpid(), 100, o, nil, newFixture(o), 1, false)
	if err == nil || res.Requests != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestNearestRank(t *testing.T) {
	if got := percentileMs([]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, int64(time.Second)}, 95); got != 1000 {
		t.Fatal(got)
	}
}

func TestStorageDropDeltaRequiresOneEnabledEpoch(t *testing.T) {
	before := storageState{FeedID: "same-process"}
	before.Storage.Enabled, before.Storage.Dropped = true, 7
	for _, tc := range []struct {
		name    string
		feed    string
		enabled bool
		dropped uint64
		want    uint64
		valid   bool
	}{
		{"no loss", "same-process", true, 7, 0, true},
		{"confirmed loss", "same-process", true, 12, 5, true},
		{"counter regression", "same-process", true, 6, 0, false},
		{"restart", "new-process", true, 12, 0, false},
		{"missing epoch", "", true, 12, 0, false},
		{"disabled storage", "same-process", false, 12, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := storageState{FeedID: tc.feed}
			after.Storage.Enabled, after.Storage.Dropped = tc.enabled, tc.dropped
			got, err := storageDropDelta(before, after)
			if (err == nil) != tc.valid || got != tc.want {
				t.Fatalf("delta=%d err=%v, want=%d valid=%v", got, err, tc.want, tc.valid)
			}
		})
	}
	before.Storage.Enabled = false
	if _, err := storageDropDelta(before, before); err == nil {
		t.Fatal("missing initial storage signal was accepted")
	}
}
