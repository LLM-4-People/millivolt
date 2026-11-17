package plugins

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	maxTrustBytes      = 16 << 10
	maxLockBytes       = 128 << 10
	maxTrustKeys       = 64
	maxPinFiles        = 64
	maxInstallSource   = 512
	maxInstallStateStr = 256
)

var packagePinPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}\.mvplugin$`)

// SigningPayload is the exact byte sequence a signer signs. Both core and SDK
// must use this order; adding a field requires an ABI version bump.
func (m *PluginManifest) SigningPayload() []byte {
	if m == nil {
		return nil
	}
	return []byte(strings.Join([]string{
		m.ID,
		m.Version,
		m.ABI,
		m.MinMillivolt,
		m.Module,
		m.ModuleSHA256,
		strings.Join(m.Capabilities, ","),
		m.License,
	}, "\n"))
}

type trustFile struct {
	Keys []trustKey `json:"keys"`
}

type trustKey struct {
	ID            string `json:"id"`
	Ed25519Public string `json:"ed25519_public"`
}

// Trust is the bounded set of signers this process accepts. An empty trust
// store is development mode: unsigned manifests are allowed.
type Trust struct {
	keys map[string]ed25519.PublicKey
}

// LoadTrust reads one bounded JSON document. An empty path means no verifiers.
func LoadTrust(path string) (*Trust, error) {
	trust := &Trust{keys: map[string]ed25519.PublicKey{}}
	if path == "" {
		return trust, nil
	}
	raw, err := readBoundedFile(path, maxTrustBytes)
	if err != nil {
		return nil, fmt.Errorf("plugin trust %q: %w", path, err)
	}
	var file trustFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("plugin trust %q: %w", path, err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("plugin trust %q: trailing JSON content", path)
	}
	if len(file.Keys) > maxTrustKeys {
		return nil, fmt.Errorf("plugin trust %q: more than %d keys", path, maxTrustKeys)
	}
	for i, key := range file.Keys {
		if !pluginIDPattern.MatchString(key.ID) {
			return nil, fmt.Errorf("plugin trust[%d]: invalid id", i)
		}
		decoded, err := hex.DecodeString(key.Ed25519Public)
		if err != nil || len(decoded) != ed25519.PublicKeySize || strings.ToLower(key.Ed25519Public) != key.Ed25519Public {
			return nil, fmt.Errorf("plugin trust[%d]: ed25519_public must be %d lowercase hex characters", i, ed25519.PublicKeySize*2)
		}
		if _, exists := trust.keys[key.ID]; exists {
			return nil, fmt.Errorf("plugin trust[%d]: duplicate id %q", i, key.ID)
		}
		trust.keys[key.ID] = ed25519.PublicKey(decoded)
	}
	return trust, nil
}

// Configured reports whether at least one verifier was loaded.
func (t *Trust) Configured() bool {
	return t != nil && len(t.keys) > 0
}

// Verify enforces signatures when trust is configured. With no trust store,
// unsigned development manifests remain loadable.
func (t *Trust) Verify(m *PluginManifest) error {
	if !t.Configured() {
		return nil
	}
	if m == nil || m.Signer == "" || m.Signature == "" {
		return fmt.Errorf("manifest is unsigned but plugin trust is configured")
	}
	key, ok := t.keys[m.Signer]
	if !ok {
		return fmt.Errorf("manifest signer %q is not trusted", m.Signer)
	}
	sig, err := hex.DecodeString(m.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("manifest signature is malformed")
	}
	if !ed25519.Verify(key, m.SigningPayload(), sig) {
		return fmt.Errorf("manifest signature is invalid")
	}
	return nil
}

type lockFile struct {
	Pins     map[string]string       `json:"pins"`
	Installs map[string]InstallState `json:"installs,omitempty"`
}

// PreviousInstallState records the last known-good archive before an update.
type PreviousInstallState struct {
	Version       string `json:"version"`
	Archive       string `json:"archive"`
	ArchiveSHA256 string `json:"archive_sha256"`
	Source        string `json:"source"`
}

// InstallState is the bounded install record for one active plugin archive.
type InstallState struct {
	Version         string                `json:"version"`
	Archive         string                `json:"archive"`
	Source          string                `json:"source"`
	ResolvedVersion string                `json:"resolved_version"`
	ArchiveSHA256   string                `json:"archive_sha256"`
	InstalledAt     string                `json:"installed_at"`
	Previous        *PreviousInstallState `json:"previous,omitempty"`
}

// Locks is a filename→digest pin set plus bounded install metadata. A missing
// pin means unpinned; a present pin must match the archive or module exactly.
type Locks struct {
	Pins     map[string]string
	Installs map[string]InstallState
}

// LoadLocks reads one bounded pin document. An empty path means no pins.
func LoadLocks(path string) (*Locks, error) {
	locks := &Locks{Pins: map[string]string{}, Installs: map[string]InstallState{}}
	if path == "" {
		return locks, nil
	}
	raw, err := readBoundedFile(path, maxLockBytes)
	if err != nil {
		return nil, fmt.Errorf("plugin locks %q: %w", path, err)
	}
	var file lockFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("plugin locks %q: %w", path, err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("plugin locks %q: trailing JSON content", path)
	}
	locks.Pins = file.Pins
	if locks.Pins == nil {
		locks.Pins = map[string]string{}
	}
	locks.Installs = file.Installs
	if locks.Installs == nil {
		locks.Installs = map[string]InstallState{}
	}
	if err := locks.validate(); err != nil {
		return nil, fmt.Errorf("plugin locks %q: %w", path, err)
	}
	return locks, nil
}

func (l *Locks) validate() error {
	if l == nil {
		return fmt.Errorf("nil locks")
	}
	if len(l.Pins) > maxPinFiles {
		return fmt.Errorf("more than %d pins", maxPinFiles)
	}
	for name, digest := range l.Pins {
		if !validPinName(name) || !pluginValidDigest(digest) {
			return fmt.Errorf("invalid pin %q", name)
		}
	}
	if len(l.Installs) > maxPinFiles {
		return fmt.Errorf("more than %d installs", maxPinFiles)
	}
	for id, state := range l.Installs {
		if err := state.Validate(id); err != nil {
			return fmt.Errorf("install %q: %w", id, err)
		}
		if want, ok := l.Pins[state.Archive]; ok && want != state.ArchiveSHA256 {
			return fmt.Errorf("install %q pin does not match its archive digest", id)
		}
	}
	return nil
}

// Validate checks one install record at the config boundary.
func (s InstallState) Validate(id string) error {
	if !pluginIDPattern.MatchString(id) {
		return fmt.Errorf("invalid id")
	}
	if want := id + ".mvplugin"; s.Archive != want {
		return fmt.Errorf("archive must be %q", want)
	}
	if !pluginSemverPattern.MatchString(s.Version) {
		return fmt.Errorf("version must be a semantic version")
	}
	if len(s.Source) == 0 || len(s.Source) > maxInstallSource {
		return fmt.Errorf("source must be 1..%d bytes", maxInstallSource)
	}
	if s.ResolvedVersion != "" && !pluginSemverPattern.MatchString(s.ResolvedVersion) {
		return fmt.Errorf("resolved_version must be a semantic version")
	}
	if !pluginValidDigest(s.ArchiveSHA256) {
		return fmt.Errorf("archive_sha256 must be lowercase hex")
	}
	if len(s.InstalledAt) == 0 || len(s.InstalledAt) > maxInstallStateStr {
		return fmt.Errorf("installed_at is out of range")
	}
	if _, err := time.Parse(time.RFC3339, s.InstalledAt); err != nil {
		return fmt.Errorf("installed_at must be RFC3339")
	}
	if s.Previous == nil {
		return nil
	}
	return s.Previous.Validate(id)
}

// Validate checks one previous-install record at the config boundary.
func (s *PreviousInstallState) Validate(id string) error {
	if s == nil {
		return fmt.Errorf("nil previous install")
	}
	if !pluginSemverPattern.MatchString(s.Version) {
		return fmt.Errorf("previous version must be a semantic version")
	}
	if want := previousArchiveName(id); s.Archive != want {
		return fmt.Errorf("previous archive must be %q", want)
	}
	if !pluginValidDigest(s.ArchiveSHA256) {
		return fmt.Errorf("previous archive_sha256 must be lowercase hex")
	}
	if len(s.Source) == 0 || len(s.Source) > maxInstallSource {
		return fmt.Errorf("previous source must be 1..%d bytes", maxInstallSource)
	}
	return nil
}

func previousArchiveName(id string) string { return id + ".mvplugin.prev" }

func validPinName(name string) bool {
	return pluginModulePattern.MatchString(name) || packagePinPattern.MatchString(name)
}

func pluginValidDigest(digest string) bool {
	if len(digest) != sha256.Size*2 || strings.ToLower(digest) != digest {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size
}

// Check enforces an existing pin without adding one.
func (l *Locks) Check(name, digest string) error {
	if l == nil || l.Pins == nil {
		return nil
	}
	want, ok := l.Pins[name]
	if !ok {
		return nil
	}
	if want != digest {
		return fmt.Errorf("plugin %q is pinned to %s, found %s", name, want, digest)
	}
	return nil
}

// Set records the current digest for one module or archive.
func (l *Locks) Set(name, digest string) {
	_ = l.SetPin(name, digest)
}

// SetPin validates and records one module or archive digest.
func (l *Locks) SetPin(name, digest string) error {
	if l == nil {
		return fmt.Errorf("nil locks")
	}
	if !validPinName(name) || !pluginValidDigest(digest) {
		return fmt.Errorf("invalid pin %q", name)
	}
	if l.Pins == nil {
		l.Pins = map[string]string{}
	}
	if _, exists := l.Pins[name]; !exists && len(l.Pins) >= maxPinFiles {
		return fmt.Errorf("more than %d pins", maxPinFiles)
	}
	l.Pins[name] = digest
	return nil
}

// RemovePin removes one pin.
func (l *Locks) RemovePin(name string) {
	if l != nil {
		delete(l.Pins, name)
	}
}

// Install returns the recorded state for one plugin id.
func (l *Locks) Install(id string) (InstallState, bool) {
	if l == nil {
		return InstallState{}, false
	}
	state, ok := l.Installs[id]
	return state, ok
}

// SetInstall validates and records one plugin install state.
func (l *Locks) SetInstall(id string, state InstallState) error {
	if l == nil {
		return fmt.Errorf("nil locks")
	}
	if err := state.Validate(id); err != nil {
		return err
	}
	if l.Installs == nil {
		l.Installs = map[string]InstallState{}
	}
	if _, exists := l.Installs[id]; !exists && len(l.Installs) >= maxPinFiles {
		return fmt.Errorf("more than %d installs", maxPinFiles)
	}
	l.Installs[id] = state
	return nil
}

// RemoveInstall removes one plugin install record.
func (l *Locks) RemoveInstall(id string) {
	if l != nil {
		delete(l.Installs, id)
	}
}

// Clone makes a private deep copy so failed commits cannot mutate live state.
func (l *Locks) Clone() *Locks {
	out := &Locks{Pins: map[string]string{}, Installs: map[string]InstallState{}}
	if l == nil {
		return out
	}
	for name, digest := range l.Pins {
		out.Pins[name] = digest
	}
	for id, state := range l.Installs {
		if state.Previous != nil {
			previous := *state.Previous
			state.Previous = &previous
		}
		out.Installs[id] = state
	}
	return out
}

// Save atomically replaces the lock file with private permissions.
func (l *Locks) Save(path string) error {
	tmp, err := l.writeTemp(path)
	if err != nil {
		return err
	}
	return commitLockTemp(tmp, path)
}

func (l *Locks) writeTemp(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("plugin lock path is empty")
	}
	if l == nil {
		l = &Locks{}
	}
	if l.Pins == nil {
		l.Pins = map[string]string{}
	}
	if l.Installs == nil {
		l.Installs = map[string]InstallState{}
	}
	if err := l.validate(); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(lockFile{Pins: l.Pins, Installs: l.Installs}); err != nil {
		return "", err
	}
	if buf.Len() > maxLockBytes {
		return "", fmt.Errorf("plugin lock document exceeds the %d byte cap", maxLockBytes)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

func commitLockTemp(tmp, path string) error {
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
