package mcp

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestScopeSetAllows(t *testing.T) {
	if !AllCapabilities().Allows(CapabilityAdminHistory) {
		t.Fatal("full operator credential must allow every scope")
	}
	if AllCapabilities().Key() != "all" {
		t.Fatalf("full scope key = %q", AllCapabilities().Key())
	}
	set, err := ParseCapabilities([]string{"read:metrics", "read:metrics", "read:records"})
	if err != nil {
		t.Fatal(err)
	}
	if !set.Allows(CapabilityMetrics) || !set.Allows(CapabilityRecords) || set.Allows(CapabilityAdminHistory) {
		t.Fatalf("unexpected grants: %v", set.Strings())
	}
	if got := strings.Join(set.Strings(), ","); got != "read:metrics,read:records" {
		t.Fatalf("scope strings = %q", got)
	}
	if set.Key() != "read:metrics,read:records" {
		t.Fatalf("scope key = %q", set.Key())
	}
	empty := CapabilitySet{}
	if empty.Allows(CapabilityMetrics) || CapabilitiesFromContext(context.Background()).Allows(CapabilityMetrics) {
		t.Fatal("missing or empty scopes must not inherit full access")
	}
}

func TestParseScopesRefusesUnknown(t *testing.T) {
	if _, err := ParseCapabilities([]string{"read:everything"}); err == nil || !strings.Contains(err.Error(), "unknown capability") {
		t.Fatalf("ParseCapabilities error = %v, want unknown capability", err)
	}
}

func TestScopedServiceRegistersOnlyGrantedTools(t *testing.T) {
	const token = "0123456789abcdef"
	metricsOnly, err := ParseCapabilities([]string{"read:metrics"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithTransportAndScopes("http://127.0.0.1:8081", token, defaultTestLimits(), nil, metricsOnly)
	if err != nil {
		t.Fatal(err)
	}
	session := connect(t, service)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	for _, want := range []string{"describe", "snapshot", "values"} {
		if !slices.Contains(names, want) {
			t.Errorf("granted tool %q missing from %v", want, names)
		}
	}
	for _, forbidden := range []string{"query", "set_config", "purge", "audit_start", "config_get"} {
		if slices.Contains(names, forbidden) {
			t.Errorf("ungranted tool %q leaked into %v", forbidden, names)
		}
	}
}

func TestScopeChangesServerCacheKey(t *testing.T) {
	const token = "0123456789abcdef"
	metricsOnly, _ := ParseCapabilities([]string{"read:metrics"})
	recordsOnly, _ := ParseCapabilities([]string{"read:records"})
	a, err := NewServiceWithTransportAndScopes("http://127.0.0.1:8081", token, defaultTestLimits(), nil, metricsOnly)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewServiceWithTransportAndScopes("http://127.0.0.1:8081", token, defaultTestLimits(), nil, recordsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if a.credentialKey() == b.credentialKey() {
		t.Fatal("different scope sets must not share a cached server")
	}
	if a.credentialKey() == token {
		t.Fatal("cache key must not be the raw credential")
	}
}
