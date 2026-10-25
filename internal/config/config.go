package config

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// provPathRE is the dotted JSON path shape every provider field-map value
// must match (object segments, optionally indexed by numeric array
// segments). Shared by cost_keys, usage_keys, and models_keys validation so
// all three reject garbage at the load boundary with one rule.
var provPathRE = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)

// canonicalUsageFieldSet is the lookup-set form of
// metrics.CanonicalUsageFields - the single owner of the canonical usage
// field names (served to the Settings UI dropdown, consumed by ParseUsage).
// usage_keys keys are checked against it so the load boundary accepts
// exactly what the UI offers and what ParseUsage reads; a typo'd canonical
// name would otherwise be a silent runtime no-op.
var canonicalUsageFieldSet = func() map[string]bool {
	set := make(map[string]bool, len(metrics.CanonicalUsageFields))
	for _, f := range metrics.CanonicalUsageFields {
		set[f] = true
	}
	return set
}()

// checkProvPath rejects a provider field-map entry whose value is not a
// dotted JSON path (provPathRE). label and slot locate the entry in the
// error ("providers.<label>.<slot>"); name is the map key the path belongs
// to, empty for the slice-shaped cost_keys.
func checkProvPath(label, slot, name, path string) error {
	if !provPathRE.MatchString(path) {
		if name == "" {
			return fmt.Errorf("providers.%s.%s: %q is not a dotted JSON path", label, slot, path)
		}
		return fmt.Errorf("providers.%s.%s: %s: %q is not a dotted JSON path", label, slot, name, path)
	}
	return nil
}

// sortedKeys returns m's keys in sorted order, for error-reporting loops over
// Go maps: map iteration order is randomized, so an unsorted loop reports a
// nondeterministic first error when several entries are invalid. Every loop in
// Validate that can return an error iterates through this.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// canonicalProviderLabel is the one provider-label policy shared by the
// providers map and provider_aliases: the runtime derives a label from the
// upstream host (lowercase, no surrounding whitespace), so a configured label
// only ever matches in that canonical form. Anything else is rejected by
// validateProviderLabel rather than kept as a silent no-op.
func canonicalProviderLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}

// validateProviderLabel checks a configured provider label against the one
// canonical form. A label containing whitespace cannot be the host-derived
// value the request path records, so it is denied just like a mixed-case
// spelling. normalizeProviderAliases calls it after canonicalization;
// Validate calls it directly because provider labels are rejected, not
// rewritten.
func validateProviderLabel(label string) error {
	if label == "" {
		return errors.New("must be non-empty")
	}
	if strings.ContainsAny(label, " \t\r\n") {
		return fmt.Errorf("%q must not contain whitespace", label)
	}
	if label != strings.ToLower(label) {
		return fmt.Errorf("%q must be lowercase", label)
	}
	return nil
}

// normalizeProviderAliases is the one normalizer for provider_aliases. Both
// sides are canonicalized before the self/chain checks, and a collision created
// by that canonicalization is rejected instead of letting map iteration choose
// a winner. It is idempotent, so Apply and the shared Validate boundary may
// both call it without changing a valid map.
func normalizeProviderAliases(aliases map[string]string) (map[string]string, error) {
	if aliases == nil {
		return nil, nil
	}
	out := make(map[string]string, len(aliases))
	seen := make(map[string]string, len(aliases))
	for _, rawFrom := range sortedKeys(aliases) {
		from := canonicalProviderLabel(rawFrom)
		to := canonicalProviderLabel(aliases[rawFrom])
		if err := validateProviderLabel(from); err != nil {
			return nil, fmt.Errorf("alias label %q: %w", from, err)
		}
		if err := validateProviderLabel(to); err != nil {
			return nil, fmt.Errorf("%s: target label: %w", from, err)
		}
		if first, duplicate := seen[from]; duplicate {
			return nil, fmt.Errorf("%q and %q: labels collide after normalization to %q", first, rawFrom, from)
		}
		seen[from] = rawFrom
		out[from] = to
	}
	return out, nil
}
