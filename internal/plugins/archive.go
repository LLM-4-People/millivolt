package plugins

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	maxPackageArchiveBytes = 32 << 20
	maxPackageEntries      = 8

	packageManifestName = "manifest.json"
	packageModuleName   = "module.wasm"
	packageLicenseName  = "LICENSE"
	packageReadmeName   = "README.md"
)

// Package is the validated in-memory form of one .mvplugin archive. The
// manifest and module slices belong to the returned value; the archive bytes
// are never retained.
type Package struct {
	Manifest      *PluginManifest
	ManifestBytes []byte
	Module        []byte
	ArchiveSHA256 string
	Entries       []string
}

// ReadPackage validates one bounded .mvplugin archive and returns its manifest
// and module bytes. It never extracts files to disk.
func ReadPackage(raw []byte) (*Package, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("plugin package: empty archive")
	}
	if len(raw) > maxPackageArchiveBytes {
		return nil, fmt.Errorf("plugin package: archive exceeds the %d byte cap", maxPackageArchiveBytes)
	}
	reader, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("plugin package: invalid zip: %w", err)
	}
	if len(reader.File) > maxPackageEntries {
		return nil, fmt.Errorf("plugin package: more than %d entries", maxPackageEntries)
	}
	files := make(map[string]*zip.File, maxPackageEntries)
	entries := make([]string, 0, maxPackageEntries)
	for _, file := range reader.File {
		if len(entries) == maxPackageEntries {
			return nil, fmt.Errorf("plugin package: more than %d entries", maxPackageEntries)
		}
		if err := validatePackageEntry(file.Name); err != nil {
			return nil, err
		}
		if !file.Mode().IsRegular() {
			return nil, fmt.Errorf("plugin package entry %q: not a regular file", packageEntryLabel(file.Name))
		}
		if _, exists := files[file.Name]; exists {
			return nil, fmt.Errorf("plugin package entry %q: duplicate", packageEntryLabel(file.Name))
		}
		files[file.Name] = file
		entries = append(entries, file.Name)
	}

	manifestFile, ok := files[packageManifestName]
	if !ok {
		return nil, fmt.Errorf("plugin package: missing %s", packageManifestName)
	}
	moduleFile, ok := files[packageModuleName]
	if !ok {
		return nil, fmt.Errorf("plugin package: missing %s", packageModuleName)
	}

	manifestBytes, err := readPackageEntry(manifestFile, MaxManifestBytes, "manifest")
	if err != nil {
		return nil, err
	}
	manifest, err := decodePackageManifest(manifestBytes)
	if err != nil {
		return nil, err
	}
	if manifest.Module != packageModuleName {
		return nil, fmt.Errorf("plugin package: manifest names module %q, want %q",
			packageEntryLabel(manifest.Module), packageModuleName)
	}

	module, err := readPackageEntry(moduleFile, maxModuleBytes, "module")
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(module)
	digest := hex.EncodeToString(sum[:])
	if digest != manifest.ModuleSHA256 {
		return nil, fmt.Errorf("plugin package: module digest mismatch")
	}

	archiveSum := sha256.Sum256(raw)
	return &Package{
		Manifest:      manifest,
		ManifestBytes: manifestBytes,
		Module:        module,
		ArchiveSHA256: hex.EncodeToString(archiveSum[:]),
		Entries:       entries,
	}, nil
}

func validatePackageEntry(name string) error {
	switch {
	case strings.HasPrefix(name, "/"):
		return fmt.Errorf("plugin package entry %q: absolute path", packageEntryLabel(name))
	case strings.Contains(name, "\\"):
		return fmt.Errorf("plugin package entry %q: backslash", packageEntryLabel(name))
	case strings.HasSuffix(name, "/"):
		return fmt.Errorf("plugin package entry %q: directory", packageEntryLabel(name))
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return fmt.Errorf("plugin package entry %q: path traversal", packageEntryLabel(name))
		}
	}
	switch name {
	case packageManifestName, packageModuleName, packageLicenseName, packageReadmeName:
		return nil
	default:
		return fmt.Errorf("plugin package entry %q: unknown name", packageEntryLabel(name))
	}
}

func readPackageEntry(file *zip.File, limit int, label string) ([]byte, error) {
	if file.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("plugin package %s: uncompressed size exceeds the %d byte cap", label, limit)
	}
	reader, err := file.Open()
	if err != nil {
		return nil, fmt.Errorf("plugin package %s: open: %w", label, err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("plugin package %s: read: %w", label, err)
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("plugin package %s: exceeds the %d byte cap", label, limit)
	}
	return raw, nil
}

func decodePackageManifest(raw []byte) (*PluginManifest, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var manifest PluginManifest
	if err := dec.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("plugin package manifest: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("plugin package manifest: trailing JSON content")
	}
	if err := manifest.Validate(); err != nil {
		return nil, fmt.Errorf("plugin package manifest: %w", err)
	}
	return &manifest, nil
}

func packageEntryLabel(name string) string {
	const maxLabelBytes = 64
	if len(name) <= maxLabelBytes {
		return name
	}
	return name[:maxLabelBytes] + "..."
}
