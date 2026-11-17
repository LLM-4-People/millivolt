package plugins

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
)

const packageTestMinMillivolt = "0.3.26"

type packageZipEntry struct {
	name string
	data []byte
	mode os.FileMode
}

func makePackageZip(t *testing.T, entries ...packageZipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		w, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func packageTestModule() []byte {
	return []byte("module bytes")
}

func packageTestManifest(t *testing.T, module []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(module)
	return []byte(fmt.Sprintf(
		`{"id":"fixture","version":"0.1.0","abi":"v1","min_millivolt":%q,`+
			`"capabilities":["log"],"module":"module.wasm","module_sha256":%q,"license":"MIT"}`,
		packageTestMinMillivolt, hex.EncodeToString(sum[:])))
}

func standardPackageEntries(t *testing.T) []packageZipEntry {
	t.Helper()
	module := packageTestModule()
	return []packageZipEntry{
		{name: packageManifestName, data: packageTestManifest(t, module)},
		{name: packageModuleName, data: module},
		{name: packageLicenseName, data: []byte("MIT")},
		{name: packageReadmeName, data: []byte("fixture")},
	}
}

func TestReadPackageValid(t *testing.T) {
	raw := makePackageZip(t, standardPackageEntries(t)...)
	pkg, err := ReadPackage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Manifest == nil || pkg.Manifest.ID != "fixture" {
		t.Fatalf("manifest = %+v", pkg.Manifest)
	}
	if !bytes.Equal(pkg.ManifestBytes, standardPackageEntries(t)[0].data) {
		t.Fatal("manifest bytes were not retained")
	}
	if !bytes.Equal(pkg.Module, packageTestModule()) {
		t.Fatalf("module = %q", pkg.Module)
	}
	wantEntries := []string{packageManifestName, packageModuleName, packageLicenseName, packageReadmeName}
	if strings.Join(pkg.Entries, ",") != strings.Join(wantEntries, ",") {
		t.Fatalf("entries = %q", pkg.Entries)
	}
	sum := sha256.Sum256(raw)
	if want := hex.EncodeToString(sum[:]); pkg.ArchiveSHA256 != want {
		t.Fatalf("archive digest = %q, want %q", pkg.ArchiveSHA256, want)
	}
}

func TestReadPackageRejectsInvalidArchives(t *testing.T) {
	module := packageTestModule()
	manifest := packageTestManifest(t, module)
	moduleSum := sha256.Sum256(module)
	moduleDigest := hex.EncodeToString(moduleSum[:])
	wrongDigest := strings.Repeat("0", 64)

	tooMany := []packageZipEntry{
		{name: "extra0", data: []byte("x")},
		{name: "extra1", data: []byte("x")},
		{name: "extra2", data: []byte("x")},
		{name: "extra3", data: []byte("x")},
		{name: "extra4", data: []byte("x")},
		{name: "extra5", data: []byte("x")},
		{name: "extra6", data: []byte("x")},
		{name: "extra7", data: []byte("x")},
		{name: "extra8", data: []byte("x")},
	}

	oversizedModule := bytes.Repeat([]byte("m"), maxModuleBytes+1)
	oversizedModuleManifest := packageTestManifest(t, oversizedModule)
	longLicense := strings.Repeat("x", MaxManifestBytes+1)
	oversizedManifest := []byte(fmt.Sprintf(
		`{"id":"fixture","version":"0.1.0","abi":"v1","min_millivolt":%q,`+
			`"capabilities":["log"],"module":"module.wasm","module_sha256":%q,"license":%q}`,
		packageTestMinMillivolt, moduleDigest, longLicense))

	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{"empty", nil, "empty archive"},
		{"not zip", []byte("not a zip"), "invalid zip"},
		{"oversized archive", make([]byte, maxPackageArchiveBytes+1), "archive exceeds"},
		{"too many entries", makePackageZip(t, tooMany...), "more than 8 entries"},
		{"duplicate entry", makePackageZip(t,
			packageZipEntry{name: packageManifestName, data: manifest},
			packageZipEntry{name: packageManifestName, data: manifest},
			packageZipEntry{name: packageModuleName, data: module}), "duplicate"},
		{"directory entry", makePackageZip(t, append(standardPackageEntries(t),
			packageZipEntry{name: "dir/", data: nil})...), "directory"},
		{"symlink entry", makePackageZip(t, append(standardPackageEntries(t)[:3],
			packageZipEntry{name: packageReadmeName, data: []byte("link"), mode: os.ModeSymlink | 0o777})...), "not a regular file"},
		{"path traversal", makePackageZip(t, append(standardPackageEntries(t),
			packageZipEntry{name: "../extra.txt", data: []byte("x")})...), "path traversal"},
		{"absolute path", makePackageZip(t, append(standardPackageEntries(t),
			packageZipEntry{name: "/extra.txt", data: []byte("x")})...), "absolute path"},
		{"backslash", makePackageZip(t, append(standardPackageEntries(t),
			packageZipEntry{name: `..\extra.txt`, data: []byte("x")})...), "backslash"},
		{"unknown entry", makePackageZip(t, append(standardPackageEntries(t),
			packageZipEntry{name: "extra.txt", data: []byte("x")})...), "unknown name"},
		{"missing manifest", makePackageZip(t,
			packageZipEntry{name: packageModuleName, data: module}), "missing manifest.json"},
		{"missing module", makePackageZip(t,
			packageZipEntry{name: packageManifestName, data: manifest}), "missing module.wasm"},
		{"manifest unknown field", makePackageZip(t,
			packageZipEntry{name: packageManifestName, data: []byte(strings.Replace(
				string(manifest), `"license":"MIT"`, `"license":"MIT","extra":true`, 1))},
			packageZipEntry{name: packageModuleName, data: module}), "unknown field"},
		{"manifest trailing json", makePackageZip(t,
			packageZipEntry{name: packageManifestName, data: append(append([]byte(nil), manifest...), []byte("{}")...)},
			packageZipEntry{name: packageModuleName, data: module}), "trailing JSON"},
		{"manifest names other module", makePackageZip(t,
			packageZipEntry{name: packageManifestName, data: []byte(strings.Replace(
				string(manifest), `"module":"module.wasm"`, `"module":"other.wasm"`, 1))},
			packageZipEntry{name: packageModuleName, data: module}), "manifest names module"},
		{"manifest digest mismatch", makePackageZip(t,
			packageZipEntry{name: packageManifestName, data: []byte(strings.Replace(
				string(manifest), moduleDigest, wrongDigest, 1))},
			packageZipEntry{name: packageModuleName, data: module}), "digest mismatch"},
		{"oversized manifest", makePackageZip(t,
			packageZipEntry{name: packageManifestName, data: oversizedManifest},
			packageZipEntry{name: packageModuleName, data: module}), "manifest: uncompressed size exceeds"},
		{"oversized module", makePackageZip(t,
			packageZipEntry{name: packageManifestName, data: oversizedModuleManifest},
			packageZipEntry{name: packageModuleName, data: oversizedModule}), "module: uncompressed size exceeds"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ReadPackage(test.raw)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want substring %q", err, test.want)
			}
		})
	}
}
