package mcp

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Capability is one permission a caller may hold. It does not replace the
// operator token: it narrows which tools are registered and which internal
// operator routes the caller may reach. It is deliberately distinct from the
// Scope filter type in scope.go, which describes traffic selection.
type Capability string

const (
	CapabilityMetrics      Capability = "read:metrics"
	CapabilityRecords      Capability = "read:records"
	CapabilityAudit        Capability = "read:audit"
	CapabilityConfig       Capability = "read:config"
	CapabilityControl      Capability = "control:pause"
	CapabilityAuditControl Capability = "control:audit"
	CapabilityAdminConfig  Capability = "admin:config"
	CapabilityAdminHistory Capability = "admin:history"
	CapabilityPluginManage Capability = "plugin:manage"
)

// validCapabilities is the closed vocabulary accepted from a credential file.
// An unknown capability is a load error, never a silently ignored string.
var validCapabilities = []Capability{
	CapabilityMetrics,
	CapabilityRecords,
	CapabilityAudit,
	CapabilityConfig,
	CapabilityControl,
	CapabilityAuditControl,
	CapabilityAdminConfig,
	CapabilityAdminHistory,
	CapabilityPluginManage,
}

// CapabilitySet is a caller's granted permissions. A nil set is the full
// operator superset; an empty non-nil set grants nothing.
type CapabilitySet map[Capability]bool

// AllCapabilities is the full operator credential's capability set.
func AllCapabilities() CapabilitySet { return nil }

// ParseCapabilities validates and deduplicates a credential file's strings.
func ParseCapabilities(values []string) (CapabilitySet, error) {
	set := make(CapabilitySet, len(values))
	for _, value := range values {
		capability := Capability(strings.TrimSpace(value))
		if !slices.Contains(validCapabilities, capability) {
			return nil, fmt.Errorf("unknown capability %q", value)
		}
		set[capability] = true
	}
	return set, nil
}

// Allows reports whether the set grants a required capability. Only the full
// operator superset (nil) bypasses this check.
func (s CapabilitySet) Allows(required Capability) bool {
	if s == nil {
		return true
	}
	return s[required]
}

// Key is the capability half of the per-credential server cache key.
func (s CapabilitySet) Key() string {
	if s == nil {
		return "all"
	}
	keys := make([]string, 0, len(s))
	for capability := range s {
		keys = append(keys, string(capability))
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

// Strings is a stable, sorted view for tests and diagnostics. It never
// includes credential material.
func (s CapabilitySet) Strings() []string {
	if s == nil {
		return []string{"*"}
	}
	out := make([]string, 0, len(s))
	for capability := range s {
		out = append(out, string(capability))
	}
	slices.Sort(out)
	return out
}

// toolCapabilities is the tool-level half of the boundary. A tool missing
// from this map is not registered for a scoped caller, so adding a tool cannot
// silently escape the capability model.
var toolCapabilities = map[string]Capability{
	"describe":            CapabilityMetrics,
	"query":               CapabilityRecords,
	"explore":             CapabilityMetrics,
	"chart":               CapabilityMetrics,
	"records":             CapabilityRecords,
	"snapshot":            CapabilityMetrics,
	"prometheus":          CapabilityMetrics,
	"audit_status":        CapabilityAudit,
	"audit_start":         CapabilityAuditControl,
	"audit_stop":          CapabilityAuditControl,
	"audit_captures_list": CapabilityAudit,
	"audit_capture_get":   CapabilityAudit,
	"operator_state":      CapabilityMetrics,
	"set_pause":           CapabilityControl,
	"set_throttle":        CapabilityControl,
	"resume_quota":        CapabilityControl,
	"set_config":          CapabilityAdminConfig,
	"reload_config":       CapabilityAdminConfig,
	"config_get":          CapabilityConfig,
	"values":              CapabilityMetrics,
	"purge_preview":       CapabilityAdminHistory,
	"purge":               CapabilityAdminHistory,
}

type capabilitiesContextKey struct{}

// WithCapabilities attaches the admitted capability set to a request context.
func WithCapabilities(ctx context.Context, capabilities CapabilitySet) context.Context {
	return context.WithValue(ctx, capabilitiesContextKey{}, capabilities)
}

// CapabilitiesFromContext returns the attached set, or an empty set (deny
// all) when none was attached. It never falls back to the full operator
// superset.
func CapabilitiesFromContext(ctx context.Context) CapabilitySet {
	if set, ok := ctx.Value(capabilitiesContextKey{}).(CapabilitySet); ok {
		return set
	}
	return CapabilitySet{}
}
