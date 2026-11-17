package config

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Rule-pack bounds. Each file is read with an explicit byte limit before it is
// parsed, the directory listing is capped, and the total number of contributed
// rules is capped independently of the per-field caps Validate already owns.
const (
	RulePackMaxFiles      = 64
	RulePackMaxBytes      = 256 << 10
	RulePackMaxEntries    = 64
	RulePackMaxAllEntries = 256
	RulePackMaxPathLen    = 4096
)

// RulePackOrigin records which pack contributed which keys. Config.Get exposes
// it through the admin handler; it is runtime metadata, not persisted policy.
type RulePackOrigin struct {
	File string   `json:"file"`
	Keys []string `json:"keys"`
}

// rulePackKeys is the whole set of config keys a pack may contribute. Adding a
// key here is a compatibility decision: it gives third-party data a new place
// to write, so new keys must still pass Validate with the same caps as the
// operator's own config.
var rulePackKeys = []string{
	"model_rules",
	"provider_aliases",
	"providers",
	"request_overrides",
	"sub_conversations",
}

// ApplyRulePacks reads direct .yaml/.yml files from base.PluginDir when
// PluginsEnabled is set, merges their allowlisted keys into a clone of base,
// and validates the merged result. A bad pack fails closed: no partial merge
// is returned and the caller's config is never mutated. Directories are read
// in sorted filename order, so rule order is deterministic.
func ApplyRulePacks(base *Config) (*Config, []RulePackOrigin, error) {
	if base == nil {
		return nil, nil, fmt.Errorf("rule packs: nil config")
	}
	out := base.Clone()
	if !base.PluginsEnabled || strings.TrimSpace(base.PluginDir) == "" {
		return out, nil, nil
	}
	if len(base.PluginDir) > RulePackMaxPathLen {
		return nil, nil, fmt.Errorf("plugin_dir: path must be at most %d bytes", RulePackMaxPathLen)
	}
	entries, err := os.ReadDir(base.PluginDir)
	if err != nil {
		return nil, nil, fmt.Errorf("plugin_dir %q: %w", base.PluginDir, err)
	}
	origins := make([]RulePackOrigin, 0)
	total := 0
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		files++
		if files > RulePackMaxFiles {
			return nil, nil, fmt.Errorf("plugin_dir: more than %d rule packs", RulePackMaxFiles)
		}
		info, err := entry.Info()
		if err != nil {
			return nil, nil, fmt.Errorf("rule pack %q: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("rule pack %q: not a regular file", name)
		}
		if info.Size() > RulePackMaxBytes {
			return nil, nil, fmt.Errorf("rule pack %q: %d bytes exceeds the %d byte cap", name, info.Size(), RulePackMaxBytes)
		}
		path := filepath.Join(base.PluginDir, name)
		body, err := readRulePack(path)
		if err != nil {
			return nil, nil, fmt.Errorf("rule pack %q: %w", name, err)
		}
		pack, skipped, err := loadYAMLBytesFailClosed(path, body, false)
		if err != nil {
			return nil, nil, fmt.Errorf("rule pack %q: %w", name, err)
		}
		if len(skipped) != 0 {
			return nil, nil, fmt.Errorf("rule pack %q: dropped keys %q", name, strings.Join(skipped, ", "))
		}
		keys, err := rulePackPresentKeys(path, body)
		if err != nil {
			return nil, nil, fmt.Errorf("rule pack %q: %w", name, err)
		}
		for _, key := range keys {
			if !slices.Contains(rulePackKeys, key) {
				return nil, nil, fmt.Errorf("rule pack %q: key %q is not allowed in a rule pack (allowed: %s)",
					name, key, strings.Join(rulePackKeys, ", "))
			}
		}
		count := rulePackEntryCount(pack, keys)
		if count > RulePackMaxEntries {
			return nil, nil, fmt.Errorf("rule pack %q: %d entries exceeds the per-pack cap of %d", name, count, RulePackMaxEntries)
		}
		total += count
		if total > RulePackMaxAllEntries {
			return nil, nil, fmt.Errorf("rule pack %q: total entries exceed the cap of %d", name, RulePackMaxAllEntries)
		}
		if err := mergeRulePack(out, pack, keys, name); err != nil {
			return nil, nil, err
		}
		origins = append(origins, RulePackOrigin{File: name, Keys: keys})
	}
	if err := out.Validate(); err != nil {
		return nil, nil, fmt.Errorf("rule packs: merged config invalid: %w", err)
	}
	out.RulePackOrigins = origins
	return out, origins, nil
}

// readRulePack bounds the read independently of the stat check: a file that
// grows between the two cannot make us allocate past the cap.
func readRulePack(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, RulePackMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > RulePackMaxBytes {
		return nil, fmt.Errorf("file exceeds the %d byte cap", RulePackMaxBytes)
	}
	return body, nil
}

func rulePackPresentKeys(path string, body []byte) ([]string, error) {
	dec := yaml.NewDecoder(bytes.NewReader(body))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil && err != io.EOF {
		return nil, parseConfig(path, err)
	}
	mapping, err := yamlDocumentMapping(&doc)
	if err != nil {
		return nil, parseConfig(path, err)
	}
	present, _, err := yamlMappingValues(mapping)
	if err != nil {
		return nil, parseConfig(path, err)
	}
	return slices.Sorted(maps.Keys(present)), nil
}

func rulePackEntryCount(pack *Config, keys []string) int {
	count := 0
	for _, key := range keys {
		switch key {
		case "model_rules":
			count += len(pack.ModelRules)
		case "provider_aliases":
			count += len(pack.ProviderAliases)
		case "providers":
			count += len(pack.Providers)
		case "request_overrides":
			count += len(pack.RequestOverrides)
		case "sub_conversations":
			count += len(pack.SubConversations)
		}
	}
	return count
}

func mergeRulePack(dst, pack *Config, keys []string, file string) error {
	for _, key := range keys {
		switch key {
		case "model_rules":
			dst.ModelRules = append(dst.ModelRules, pack.ModelRules...)
		case "request_overrides":
			dst.RequestOverrides = append(dst.RequestOverrides, pack.RequestOverrides...)
		case "sub_conversations":
			dst.SubConversations = append(dst.SubConversations, pack.SubConversations...)
		case "provider_aliases":
			merged, err := mergeRulePackMap(dst.ProviderAliases, pack.ProviderAliases, "provider_aliases", file)
			if err != nil {
				return err
			}
			dst.ProviderAliases = merged
		case "providers":
			merged, err := mergeRulePackMap(dst.Providers, pack.Providers, "providers", file)
			if err != nil {
				return err
			}
			dst.Providers = merged
		}
	}
	return nil
}

// mergeRulePackMap refuses a duplicate label rather than silently replacing an
// operator's entry. The merged map is built from the destination so a failed
// merge never mutates dst.
func mergeRulePackMap[V any](dst, src map[string]V, key, file string) (map[string]V, error) {
	merged := make(map[string]V, len(dst)+len(src))
	maps.Copy(merged, dst)
	for _, label := range slices.Sorted(maps.Keys(src)) {
		if _, exists := merged[label]; exists {
			return nil, fmt.Errorf("rule pack %q: %s.%s duplicates an existing key", file, key, label)
		}
		merged[label] = src[label]
	}
	return merged, nil
}
