package plugins

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// InstallReport is the non-secret result of one install operation.
type InstallReport struct {
	ID              string `json:"id"`
	Version         string `json:"version"`
	Archive         string `json:"archive"`
	ArchiveSHA256   string `json:"archive_sha256"`
	Signature       string `json:"signature"`
	Pin             string `json:"pin"`
	RestartRequired bool   `json:"restart_required"`
}

// InstallFile validates one local .mvplugin file and installs it as the single
// active archive for its declared plugin id.
func InstallFile(src, pluginDir, lockPath string, trust *Trust, allowUnsigned bool) (InstallReport, error) {
	return installFile(src, pluginDir, lockPath, "local", trust, allowUnsigned)
}

func installFile(src, pluginDir, lockPath, source string, trust *Trust, allowUnsigned bool) (InstallReport, error) {
	if pluginDir == "" || lockPath == "" {
		return InstallReport{}, fmt.Errorf("plugin dir and lock file are required")
	}
	if err := requirePlainDir(pluginDir); err != nil {
		return InstallReport{}, err
	}
	if err := requirePlainDir(filepath.Dir(lockPath)); err != nil {
		return InstallReport{}, err
	}
	if err := requireRegularFile(src); err != nil {
		return InstallReport{}, err
	}

	raw, err := readBoundedFile(src, maxPackageArchiveBytes)
	if err != nil {
		return InstallReport{}, fmt.Errorf("plugin source: %w", err)
	}
	pkg, err := ReadPackage(raw)
	if err != nil {
		return InstallReport{}, err
	}

	signature, err := installSignature(pkg.Manifest, trust, allowUnsigned)
	if err != nil {
		return InstallReport{}, err
	}

	id := pkg.Manifest.ID
	archiveName := id + ".mvplugin"
	if !packagePinPattern.MatchString(archiveName) {
		return InstallReport{}, fmt.Errorf("plugin id %q is not a valid archive name", id)
	}
	active := filepath.Join(pluginDir, archiveName)
	previous := filepath.Join(pluginDir, previousArchiveName(id))
	if _, err := os.Lstat(active); err == nil {
		return InstallReport{}, fmt.Errorf("plugin %q is already installed", id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return InstallReport{}, err
	}
	if _, err := os.Lstat(previous); err == nil {
		return InstallReport{}, fmt.Errorf("plugin %q has a stale previous archive", id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return InstallReport{}, err
	}

	locks, err := loadLocksForInstall(lockPath)
	if err != nil {
		return InstallReport{}, err
	}
	if _, exists := locks.Install(id); exists {
		return InstallReport{}, fmt.Errorf("plugin %q is already installed", id)
	}
	if err := locks.Check(archiveName, pkg.ArchiveSHA256); err != nil {
		return InstallReport{}, err
	}

	next := locks.Clone()
	if err := next.SetPin(archiveName, pkg.ArchiveSHA256); err != nil {
		return InstallReport{}, err
	}
	state := InstallState{
		Version:         pkg.Manifest.Version,
		Archive:         archiveName,
		Source:          source,
		ResolvedVersion: pkg.Manifest.Version,
		ArchiveSHA256:   pkg.ArchiveSHA256,
		InstalledAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := next.SetInstall(id, state); err != nil {
		return InstallReport{}, err
	}

	archiveTmp, err := stageArchiveFile(pluginDir, archiveName, raw)
	if err != nil {
		return InstallReport{}, err
	}
	committed := false
	defer func() {
		if !committed {
			os.Remove(archiveTmp)
		}
	}()

	lockTmp, err := next.writeTemp(lockPath)
	if err != nil {
		return InstallReport{}, err
	}
	defer os.Remove(lockTmp)

	if _, err := os.Lstat(active); err == nil {
		return InstallReport{}, fmt.Errorf("plugin %q is already installed", id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return InstallReport{}, err
	}
	if err := os.Rename(archiveTmp, active); err != nil {
		return InstallReport{}, err
	}
	if err := commitLockTemp(lockTmp, lockPath); err != nil {
		os.Remove(active)
		return InstallReport{}, err
	}
	committed = true
	return InstallReport{
		ID:              id,
		Version:         pkg.Manifest.Version,
		Archive:         archiveName,
		ArchiveSHA256:   pkg.ArchiveSHA256,
		Signature:       signature,
		Pin:             "updated",
		RestartRequired: true,
	}, nil
}

// Uninstall removes the active archive, the rollback archive and the install
// record for one plugin id.
func Uninstall(pluginDir, lockPath, id string) error {
	if !pluginIDPattern.MatchString(id) {
		return fmt.Errorf("invalid plugin id %q", id)
	}
	if pluginDir == "" || lockPath == "" {
		return fmt.Errorf("plugin dir and lock file are required")
	}
	if err := requirePlainDir(pluginDir); err != nil {
		return err
	}
	if err := requirePlainDir(filepath.Dir(lockPath)); err != nil {
		return err
	}

	locks, err := loadLocksForInstall(lockPath)
	if err != nil {
		return err
	}
	state, ok := locks.Install(id)
	if !ok {
		return fmt.Errorf("plugin %q is not installed", id)
	}
	active := filepath.Join(pluginDir, state.Archive)
	previous := filepath.Join(pluginDir, previousArchiveName(id))

	files := []string{active, previous}
	saved := make(map[string][]byte, len(files))
	for _, path := range files {
		raw, present, err := readInstallFile(path)
		if err != nil {
			return err
		}
		if present {
			saved[path] = raw
		}
	}

	next := locks.Clone()
	next.RemovePin(state.Archive)
	next.RemoveInstall(id)
	lockTmp, err := next.writeTemp(lockPath)
	if err != nil {
		return err
	}
	defer os.Remove(lockTmp)

	removed := map[string]bool{}
	for _, path := range files {
		if _, present := saved[path]; !present {
			continue
		}
		if err := os.Remove(path); err != nil {
			restoreInstallFiles(saved, removed)
			return err
		}
		removed[path] = true
	}
	if err := commitLockTemp(lockTmp, lockPath); err != nil {
		restoreInstallFiles(saved, removed)
		return err
	}
	return nil
}

func installSignature(manifest *PluginManifest, trust *Trust, allowUnsigned bool) (string, error) {
	if trust.Configured() {
		if err := trust.Verify(manifest); err != nil {
			return "", err
		}
		return "verified", nil
	}
	if !allowUnsigned {
		return "", fmt.Errorf("unsigned plugin packages are not allowed by this source")
	}
	if manifest.Signer != "" {
		return "unverified", nil
	}
	return "unsigned", nil
}

func loadLocksForInstall(path string) (*Locks, error) {
	locks, err := LoadLocks(path)
	if err == nil {
		return locks, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return &Locks{Pins: map[string]string{}, Installs: map[string]InstallState{}}, nil
	}
	return nil, err
}

func requirePlainDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%q is not a plain directory", path)
	}
	return nil
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%q is not a regular file", path)
	}
	return nil
}

func stageArchiveFile(dir, name string, raw []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if _, err := tmp.Write(raw); err != nil {
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

func readInstallFile(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("%q is not a regular file", path)
	}
	raw, err := readBoundedFile(path, maxPackageArchiveBytes)
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func restoreInstallFiles(saved map[string][]byte, removed map[string]bool) {
	for path, raw := range saved {
		if !removed[path] {
			continue
		}
		tmp, err := stageArchiveFile(filepath.Dir(path), filepath.Base(path), raw)
		if err != nil {
			continue
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
		}
	}
}
