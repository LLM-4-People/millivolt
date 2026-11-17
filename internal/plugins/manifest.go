package plugins

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// Plugin manifests are data-only install declarations. The field names and
// capability vocabulary mirror millivolt-plugin-sdk abi/v1; keeping the
// decoder here avoids a source dependency from the proxy image to the SDK.
const (
	MaxManifestBytes = 16 << 10
	MaxPluginFiles   = 16
	MaxCapabilities  = 16
)

var (
	pluginIDPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	pluginSemverPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)
	pluginModulePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+\.wasm$`)
)

// PluginCapabilities is the closed set of host imports a module may request.
var PluginCapabilities = []string{
	"log",
	"emit_metric",
	"read:records",
	"read:config",
	"declare_tool",
	"declare_ui_panel",
	"http:fetch",
}

// PluginManifest is the complete install-time declaration for one module.
type PluginManifest struct {
	ID           string   `json:"id"`
	Version      string   `json:"version"`
	ABI          string   `json:"abi"`
	MinMillivolt string   `json:"min_millivolt"`
	Capabilities []string `json:"capabilities"`
	Module       string   `json:"module"`
	ModuleSHA256 string   `json:"module_sha256"`
	License      string   `json:"license"`
	Signer       string   `json:"signer,omitempty"`
	Signature    string   `json:"signature,omitempty"`
}

// LoadPluginManifest reads one bounded JSON manifest and refuses trailing
// content. A malformed manifest fails boot before any code is instantiated.
func LoadPluginManifest(path string) (*PluginManifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("plugin manifest %q: %w", path, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("plugin manifest %q: %w", path, err)
	}
	if len(raw) > MaxManifestBytes {
		return nil, fmt.Errorf("plugin manifest %q: file exceeds the %d byte cap", path, MaxManifestBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m PluginManifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("plugin manifest %q: %w", path, err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("plugin manifest %q: trailing JSON content", path)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("plugin manifest %q: %w", path, err)
	}
	return &m, nil
}

// Validate checks every manifest field at the boot boundary.
func (m *PluginManifest) Validate() error {
	if m == nil {
		return fmt.Errorf("nil plugin manifest")
	}
	if !pluginIDPattern.MatchString(m.ID) {
		return fmt.Errorf("id: must be a lowercase identifier of at most 64 characters")
	}
	if !pluginSemverPattern.MatchString(m.Version) {
		return fmt.Errorf("version: must be a semantic version")
	}
	if m.ABI != "v1" {
		return fmt.Errorf("abi: must be \"v1\", got %q", m.ABI)
	}
	if !pluginSemverPattern.MatchString(m.MinMillivolt) {
		return fmt.Errorf("min_millivolt: must be a semantic version")
	}
	if len(m.Capabilities) == 0 || len(m.Capabilities) > MaxCapabilities {
		return fmt.Errorf("capabilities: must contain 1..%d entries", MaxCapabilities)
	}
	seen := map[string]bool{}
	for _, capability := range m.Capabilities {
		if seen[capability] {
			return fmt.Errorf("capabilities: duplicate %q", capability)
		}
		seen[capability] = true
		if !pluginContains(PluginCapabilities, capability) {
			return fmt.Errorf("capabilities: unknown %q", capability)
		}
	}
	if !pluginModulePattern.MatchString(m.Module) {
		return fmt.Errorf("module: must be a .wasm filename")
	}
	decoded, err := hex.DecodeString(m.ModuleSHA256)
	if err != nil || len(decoded) != 32 || strings.ToLower(m.ModuleSHA256) != m.ModuleSHA256 {
		return fmt.Errorf("module_sha256: must be 64 lowercase hex characters")
	}
	if strings.TrimSpace(m.License) == "" {
		return fmt.Errorf("license: must be a non-empty identifier")
	}
	if (m.Signer == "") != (m.Signature == "") {
		return fmt.Errorf("signer and signature must be set together")
	}
	if m.Signer != "" {
		if !pluginIDPattern.MatchString(m.Signer) {
			return fmt.Errorf("signer: must be a lowercase identifier of at most 64 characters")
		}
		sig, err := hex.DecodeString(m.Signature)
		if err != nil || len(sig) != 64 || strings.ToLower(m.Signature) != m.Signature {
			return fmt.Errorf("signature: must be 128 lowercase hex characters")
		}
	}
	return nil
}

func (m *PluginManifest) allows(capability string) bool {
	if m == nil {
		return false
	}
	return pluginContains(m.Capabilities, capability)
}

func pluginContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
