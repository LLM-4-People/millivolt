package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	millivolt "github.com/LLM-4-People/millivolt"
)

// UpdateCandidate is a newer release discovered for an installed plugin.
type UpdateCandidate struct {
	ID              string `json:"id"`
	CurrentVersion  string `json:"current_version"`
	Version         string `json:"version"`
	Source          string `json:"source"`
	ResolvedVersion string `json:"resolved_version"`
}

// CapabilityChanges reports permission changes in an update.
type CapabilityChanges struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// UpdateRequest is the operator command for one staged plugin update.
type UpdateRequest struct {
	ID                      string `json:"id"`
	Source                  string `json:"source,omitempty"`
	ConfirmCapabilityChange bool   `json:"confirm_capability_change,omitempty"`
	AllowDowngrade          bool   `json:"allow_downgrade,omitempty"`
}

// UpdateReport is the non-secret result of one staged update.
type UpdateReport struct {
	ID                string            `json:"id"`
	FromVersion       string            `json:"from_version"`
	ToVersion         string            `json:"to_version"`
	Archive           string            `json:"archive"`
	ArchiveSHA256     string            `json:"archive_sha256"`
	Signature         string            `json:"signature"`
	Pin               string            `json:"pin"`
	CapabilityChanges CapabilityChanges `json:"capability_changes"`
	RestartRequired   bool              `json:"restart_required"`
}

// RollbackReport is the non-secret result of one rollback.
type RollbackReport struct {
	ID              string `json:"id"`
	Version         string `json:"version"`
	ArchiveSHA256   string `json:"archive_sha256"`
	Signature       string `json:"signature"`
	Pin             string `json:"pin"`
	RestartRequired bool   `json:"restart_required"`
}

// CheckUpdates resolves installed plugin sources and reports newer stable
// releases. It never mutates archives or lock state.
func CheckUpdates(ctx context.Context, id string, locks *Locks, fetcher SourceFetcher) ([]UpdateCandidate, error) {
	if fetcher == nil {
		return nil, fmt.Errorf("plugin source fetcher is not configured")
	}
	state, ok := locks.Install(id)
	if !ok {
		return nil, fmt.Errorf("plugin %q is not installed", id)
	}
	if !strings.HasPrefix(state.Source, "github:") {
		return []UpdateCandidate{}, nil
	}
	ref, err := latestSource(state.Source)
	if err != nil {
		return nil, err
	}
	fetched, err := fetcher.Fetch(ctx, ref)
	if err != nil {
		return nil, err
	}
	if fetched.Package.Manifest.ID != id {
		return nil, fmt.Errorf("update id %q does not match installed plugin %q", fetched.Package.Manifest.ID, id)
	}
	if compareVersions(fetched.Package.Manifest.Version, state.Version) <= 0 {
		return []UpdateCandidate{}, nil
	}
	return []UpdateCandidate{{
		ID:              id,
		CurrentVersion:  state.Version,
		Version:         fetched.Package.Manifest.Version,
		Source:          fetched.Source,
		ResolvedVersion: fetched.ResolvedVersion,
	}}, nil
}

// Update validates one fetched release and commits it with a one-version
// rollback archive. A failed commit restores the active archive and pins.
func Update(ctx context.Context, req UpdateRequest, pluginDir, lockPath string, locks *Locks, trust *Trust, fetcher SourceFetcher) (UpdateReport, error) {
	var report UpdateReport
	if locks == nil || pluginDir == "" || lockPath == "" {
		return report, fmt.Errorf("plugin dir, lock file and state are required")
	}
	if fetcher == nil {
		return report, fmt.Errorf("plugin source fetcher is not configured")
	}
	if trust == nil || !trust.Configured() {
		return report, fmt.Errorf("remote plugin updates require plugin_trust_file")
	}
	state, ok := locks.Install(req.ID)
	if !ok {
		return report, fmt.Errorf("plugin %q is not installed", req.ID)
	}
	source := req.Source
	if source == "" {
		latest, err := latestSource(state.Source)
		if err != nil {
			return report, err
		}
		source = latest
	}
	fetched, err := fetcher.Fetch(ctx, source)
	if err != nil {
		return report, err
	}
	manifest := fetched.Package.Manifest
	if manifest.ID != req.ID {
		return report, fmt.Errorf("update id %q does not match installed plugin %q", manifest.ID, req.ID)
	}
	cmp := compareVersions(manifest.Version, state.Version)
	if cmp == 0 || (cmp < 0 && !req.AllowDowngrade) {
		return report, fmt.Errorf("update version %s is not newer than %s", manifest.Version, state.Version)
	}
	if manifest.ABI != "v1" {
		return report, fmt.Errorf("unsupported plugin ABI %q", manifest.ABI)
	}
	if compareVersions(manifest.MinMillivolt, millivolt.Version()) > 0 {
		return report, fmt.Errorf("plugin requires millivolt %s or newer", manifest.MinMillivolt)
	}
	if err := trust.Verify(manifest); err != nil {
		return report, err
	}

	activePath := filepath.Join(pluginDir, state.Archive)
	currentRaw, err := readBoundedFile(activePath, maxPackageArchiveBytes)
	if err != nil {
		return report, fmt.Errorf("current plugin archive: %w", err)
	}
	current, err := ReadPackage(currentRaw)
	if err != nil {
		return report, fmt.Errorf("current plugin archive: %w", err)
	}
	if current.Manifest.ID != req.ID {
		return report, fmt.Errorf("active archive id %q does not match %q", current.Manifest.ID, req.ID)
	}
	changes := diffCapabilities(current.Manifest.Capabilities, manifest.Capabilities)
	if len(changes.Added) > 0 && !req.ConfirmCapabilityChange {
		return report, fmt.Errorf("update adds capabilities: %s", strings.Join(changes.Added, ", "))
	}

	previousName := previousArchiveName(req.ID)
	oldPrev, hadPrev, err := readOptionalBoundedFile(filepath.Join(pluginDir, previousName))
	if err != nil {
		return report, err
	}

	newTmp, err := stageArchiveFile(pluginDir, state.Archive, fetched.Raw)
	if err != nil {
		return report, err
	}
	defer os.Remove(newTmp)
	prevTmp, err := stageArchiveFile(pluginDir, previousName, currentRaw)
	if err != nil {
		return report, err
	}
	defer os.Remove(prevTmp)

	installedAt := time.Now().UTC().Format(time.RFC3339)
	next := locks.Clone()
	if err := next.SetPin(state.Archive, fetched.Package.ArchiveSHA256); err != nil {
		return report, err
	}
	if err := next.SetInstall(req.ID, InstallState{
		Version:         manifest.Version,
		Archive:         state.Archive,
		Source:          fetched.Source,
		ResolvedVersion: fetched.ResolvedVersion,
		ArchiveSHA256:   fetched.Package.ArchiveSHA256,
		InstalledAt:     installedAt,
		Previous: &PreviousInstallState{
			Version:       state.Version,
			Archive:       previousName,
			ArchiveSHA256: current.ArchiveSHA256,
			Source:        state.Source,
		},
	}); err != nil {
		return report, err
	}
	lockTmp, err := next.writeTemp(lockPath)
	if err != nil {
		return report, err
	}
	defer os.Remove(lockTmp)

	if err := replaceArchiveFile(pluginDir, previousName, oldPrev, hadPrev, prevTmp); err != nil {
		return report, err
	}
	if err := replaceArchiveFile(pluginDir, state.Archive, currentRaw, true, newTmp); err != nil {
		restoreArchiveFile(pluginDir, previousName, oldPrev, hadPrev)
		return report, err
	}
	if err := commitLockTemp(lockTmp, lockPath); err != nil {
		restoreArchiveFile(pluginDir, state.Archive, currentRaw, true)
		restoreArchiveFile(pluginDir, previousName, oldPrev, hadPrev)
		return report, err
	}
	return UpdateReport{
		ID:                req.ID,
		FromVersion:       state.Version,
		ToVersion:         manifest.Version,
		Archive:           state.Archive,
		ArchiveSHA256:     fetched.Package.ArchiveSHA256,
		Signature:         "verified",
		Pin:               "updated",
		CapabilityChanges: changes,
		RestartRequired:   true,
	}, nil
}

// Rollback restores the recorded previous archive and pin.
func Rollback(pluginDir, lockPath string, locks *Locks, id string, trust *Trust) (RollbackReport, error) {
	var report RollbackReport
	if locks == nil || pluginDir == "" || lockPath == "" {
		return report, fmt.Errorf("plugin dir, lock file and state are required")
	}
	state, ok := locks.Install(id)
	if !ok {
		return report, fmt.Errorf("plugin %q is not installed", id)
	}
	if state.Previous == nil {
		return report, fmt.Errorf("plugin %q has no previous version", id)
	}
	prevPath := filepath.Join(pluginDir, state.Previous.Archive)
	prevRaw, err := readBoundedFile(prevPath, maxPackageArchiveBytes)
	if err != nil {
		return report, fmt.Errorf("previous archive: %w", err)
	}
	prevPkg, err := ReadPackage(prevRaw)
	if err != nil {
		return report, err
	}
	if prevPkg.Manifest.ID != id || prevPkg.ArchiveSHA256 != state.Previous.ArchiveSHA256 {
		return report, fmt.Errorf("previous archive does not match install state")
	}
	if err := trust.Verify(prevPkg.Manifest); err != nil {
		return report, err
	}

	activePath := filepath.Join(pluginDir, state.Archive)
	currentRaw, err := readBoundedFile(activePath, maxPackageArchiveBytes)
	if err != nil {
		return report, fmt.Errorf("active archive: %w", err)
	}
	next := locks.Clone()
	if err := next.SetPin(state.Archive, prevPkg.ArchiveSHA256); err != nil {
		return report, err
	}
	if err := next.SetInstall(id, InstallState{
		Version:         state.Previous.Version,
		Archive:         state.Archive,
		Source:          state.Previous.Source,
		ResolvedVersion: state.Previous.Version,
		ArchiveSHA256:   prevPkg.ArchiveSHA256,
		InstalledAt:     time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return report, err
	}
	lockTmp, err := next.writeTemp(lockPath)
	if err != nil {
		return report, err
	}
	defer os.Remove(lockTmp)
	activeTmp, err := stageArchiveFile(pluginDir, state.Archive, prevRaw)
	if err != nil {
		return report, err
	}
	defer os.Remove(activeTmp)
	if err := replaceArchiveFile(pluginDir, state.Archive, currentRaw, true, activeTmp); err != nil {
		return report, err
	}
	if err := commitLockTemp(lockTmp, lockPath); err != nil {
		restoreArchiveFile(pluginDir, state.Archive, currentRaw, true)
		return report, err
	}
	if err := os.Remove(prevPath); err != nil && !os.IsNotExist(err) {
		return report, err
	}
	return RollbackReport{
		ID:              id,
		Version:         state.Previous.Version,
		ArchiveSHA256:   prevPkg.ArchiveSHA256,
		Signature:       "verified",
		Pin:             "updated",
		RestartRequired: true,
	}, nil
}

func diffCapabilities(current, candidate []string) CapabilityChanges {
	have := make(map[string]bool, len(current))
	for _, capability := range current {
		have[capability] = true
	}
	want := make(map[string]bool, len(candidate))
	for _, capability := range candidate {
		want[capability] = true
	}
	var changes CapabilityChanges
	for capability := range want {
		if !have[capability] {
			changes.Added = append(changes.Added, capability)
		}
	}
	for capability := range have {
		if !want[capability] {
			changes.Removed = append(changes.Removed, capability)
		}
	}
	sort.Strings(changes.Added)
	sort.Strings(changes.Removed)
	return changes
}

func compareVersions(a, b string) int {
	pa, err := parseVersion(a)
	if err != nil {
		return strings.Compare(a, b)
	}
	pb, err := parseVersion(b)
	if err != nil {
		return strings.Compare(a, b)
	}
	for i := 0; i < 3; i++ {
		if pa.core[i] < pb.core[i] {
			return -1
		}
		if pa.core[i] > pb.core[i] {
			return 1
		}
	}
	if len(pa.pre) == 0 && len(pb.pre) == 0 {
		return 0
	}
	if len(pa.pre) == 0 {
		return 1
	}
	if len(pb.pre) == 0 {
		return -1
	}
	for i := 0; i < len(pa.pre) && i < len(pb.pre); i++ {
		if cmp := comparePreRelease(pa.pre[i], pb.pre[i]); cmp != 0 {
			return cmp
		}
	}
	if len(pa.pre) < len(pb.pre) {
		return -1
	}
	if len(pa.pre) > len(pb.pre) {
		return 1
	}
	return 0
}

type parsedVersion struct {
	core [3]uint64
	pre  []string
}

func parseVersion(raw string) (parsedVersion, error) {
	var out parsedVersion
	value := strings.TrimPrefix(raw, "v")
	if i := strings.IndexByte(value, '+'); i >= 0 {
		value = value[:i]
	}
	core := value
	if i := strings.IndexByte(value, '-'); i >= 0 {
		core = value[:i]
		out.pre = strings.Split(value[i+1:], ".")
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("invalid version %q", raw)
	}
	for i := 0; i < 3; i++ {
		n, err := strconv.ParseUint(parts[i], 10, 64)
		if err != nil {
			return out, fmt.Errorf("invalid version %q", raw)
		}
		out.core[i] = n
	}
	return out, nil
}

func comparePreRelease(a, b string) int {
	an, aErr := strconv.ParseUint(a, 10, 64)
	bn, bErr := strconv.ParseUint(b, 10, 64)
	switch {
	case aErr == nil && bErr == nil:
		if an < bn {
			return -1
		}
		if an > bn {
			return 1
		}
		return 0
	case aErr == nil:
		return -1
	case bErr == nil:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func readOptionalBoundedFile(path string) ([]byte, bool, error) {
	raw, err := readBoundedFile(path, maxPackageArchiveBytes)
	if err == nil {
		return raw, true, nil
	}
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return nil, false, err
}

func replaceArchiveFile(dir, name string, current []byte, hadCurrent bool, staged string) error {
	path := filepath.Join(dir, name)
	if hadCurrent {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(staged, path); err != nil {
		if hadCurrent {
			restoreArchiveFile(dir, name, current, true)
		}
		return err
	}
	return nil
}

func restoreArchiveFile(dir, name string, raw []byte, had bool) {
	if !had {
		return
	}
	tmp, err := stageArchiveFile(dir, name, raw)
	if err != nil {
		return
	}
	path := filepath.Join(dir, name)
	_ = os.Remove(path)
	_ = os.Rename(tmp, path)
}
