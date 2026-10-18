package backup

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"

	"github.com/LLM-4-People/millivolt/internal/config"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	var yaml bytes.Buffer
	if err := config.WriteYAML(&yaml, config.Default()); err != nil {
		t.Fatal(err)
	}
	db := packedRequestsDB(t)
	stage := t.TempDir()
	raw, err := Encode(stage, Archive{Created: time.Unix(1_700_000_000, 0).UTC(), Config: yaml.Bytes(), Database: db})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(stage, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Config, yaml.Bytes()) || !bytes.Equal(got.Database, db) {
		t.Fatal("round-trip changed members")
	}
	if got.Created.Unix() != 1_700_000_000 {
		t.Fatalf("created = %s", got.Created)
	}
}

func TestEncodeConfigOnlyAndDatabaseOnly(t *testing.T) {
	var yaml bytes.Buffer
	if err := config.WriteYAML(&yaml, config.Default()); err != nil {
		t.Fatal(err)
	}
	cfgRaw, err := Encode("", Archive{Config: yaml.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(cfgRaw)
	if err != nil || len(got.Database) != 0 || !bytes.Equal(got.Config, yaml.Bytes()) {
		t.Fatalf("config-only: %v len(db)=%d", err, len(got.Database))
	}
	dbRaw, err := Encode(t.TempDir(), Archive{Database: packedRequestsDB(t)})
	if err != nil {
		t.Fatal(err)
	}
	got, err = Decode(dbRaw)
	if err != nil || len(got.Config) != 0 || len(got.Database) == 0 {
		t.Fatalf("database-only: %v", err)
	}
}

func TestDecodeRejectsCorruption(t *testing.T) {
	var yaml bytes.Buffer
	if err := config.WriteYAML(&yaml, config.Default()); err != nil {
		t.Fatal(err)
	}
	raw, err := Encode("", Archive{Config: yaml.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	trunc := append([]byte(nil), raw[:len(raw)-8]...)
	if _, err := Decode(trunc); err == nil {
		t.Fatal("truncated archive accepted")
	}
	flipped := append([]byte(nil), raw...)
	flipped[len(flipped)/2] ^= 0xff
	if _, err := Decode(flipped); err == nil {
		t.Fatal("bit-flipped archive accepted")
	}
	if _, err := Decode([]byte("not a backup")); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := Encode("", Archive{}); err == nil {
		t.Fatal("empty archive encoded")
	}
}

func TestValidateRejectsInvalidMembers(t *testing.T) {
	if err := Validate("", Archive{Config: []byte("listen: 8080\n")}); err == nil {
		t.Fatal("invalid config admitted")
	}
	if err := Validate(t.TempDir(), Archive{Database: []byte("SQLite format 3\x00not-a-db")}); err == nil {
		t.Fatal("invalid sqlite admitted")
	}
}

// TestValidateRejectsEmptyArchive pins Validate's no-member door: the same
// named ErrEmptyArchive Encode returns, classified via errors.Is, so a caller
// can tell an empty archive apart from invalid members. Encode's door is
// pinned by the corruption suite; Validate's had no coverage.
func TestValidateRejectsEmptyArchive(t *testing.T) {
	if err := Validate("", Archive{}); !errors.Is(err, ErrEmptyArchive) {
		t.Fatalf("Validate on an empty archive = %v, want ErrEmptyArchive", err)
	}
}

func TestCheckDatabaseDistinguishesInvalidFromIO(t *testing.T) {
	stage := t.TempDir()
	if err := CheckDatabase(stage, []byte("not sqlite")); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("header reject: %v", err)
	}
	if err := CheckDatabase(stage, []byte("SQLite format 3\x00not-a-db")); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("integrity reject: %v", err)
	}
	data := packedRequestsDB(t)
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := CheckDatabase(blocked, data)
	if err == nil {
		t.Fatal("CheckDatabase succeeded with a staging directory that cannot hold a temp file")
	}
	if errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("verify I/O reported as invalid snapshot: %v", err)
	}
	// StageSnapshotError owns the wrap: the message names the staging
	// directory (the capacity owner) and the %w keeps the underlying I/O
	// error classifiable by callers.
	if !strings.HasPrefix(err.Error(), "stage snapshot in "+blocked+": ") {
		t.Fatalf("error %q should open with the StageSnapshotError wrap naming the staging directory", err)
	}
	var perr *os.PathError
	if !errors.As(err, &perr) {
		t.Fatalf("error %q must wrap the underlying *os.PathError so callers can classify I/O failures", err)
	}
}

func TestCheckDatabaseFileInspectsInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.db")
	if err := os.WriteFile(path, packedRequestsDB(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckDatabaseFile(path); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), "millivolt-backup-db-") {
			t.Fatalf("CheckDatabaseFile staged a copy: %s", e.Name())
		}
	}
	if err := CheckDatabaseFile(""); err == nil || errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("CheckDatabaseFile accepted an empty path: %v", err)
	}
	if err := CheckDatabaseFile(filepath.Join(dir, "missing.db")); err == nil || errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("CheckDatabaseFile missing file: %v", err)
	}
	bad := filepath.Join(dir, "bad.db")
	if err := os.WriteFile(bad, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckDatabaseFile(bad); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("garbage file: %v", err)
	}
}

func TestDatabaseInspectionRequiresStagingDirectory(t *testing.T) {
	data := packedRequestsDB(t)
	if err := CheckDatabase("", data); err == nil || errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("CheckDatabase accepted an empty staging directory: %v", err)
	}
	if _, err := InspectDatabase("", data); err == nil {
		t.Fatal("InspectDatabase accepted an empty staging directory")
	}
	if err := Validate("", Archive{Database: data}); err == nil {
		t.Fatal("Validate accepted a database member without a staging directory")
	}
	var yaml bytes.Buffer
	if err := config.WriteYAML(&yaml, config.Default()); err != nil {
		t.Fatal(err)
	}
	if err := Validate("", Archive{Config: yaml.Bytes()}); err != nil {
		t.Fatalf("config-only archive must not need a staging directory: %v", err)
	}
}

func TestDecodeRejectsOversizePayload(t *testing.T) {
	const n = 100_000
	payload := append([]byte{memberConfig}, binary.BigEndian.AppendUint64(nil, uint64(n))...)
	payload = append(payload, bytes.Repeat([]byte("x"), n)...)
	sum := sha256.Sum256(payload)
	var zbuf bytes.Buffer
	enc, err := zstd.NewWriter(&zbuf, zstd.WithEncoderCRC(true), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	raw := append([]byte(magic), archiveVersion, flagConfig, 0, 0)
	raw = binary.BigEndian.AppendUint64(raw, 0)
	raw = append(raw, sum[:]...)
	raw = append(raw, zbuf.Bytes()...)
	old := payloadMax
	payloadMax = 1024
	t.Cleanup(func() { payloadMax = old })
	if _, err := Decode(raw); err == nil {
		t.Fatal("payload larger than the decode cap was admitted")
	}
	payloadMax = old
	if _, err := Decode(raw); err != nil {
		t.Fatalf("payload under Schema max should decode: %v", err)
	}
}

func TestReadCappedDoesNotDrainOversizeSource(t *testing.T) {
	src := bytes.NewReader(bytes.Repeat([]byte("y"), 8<<20))
	if _, err := readCapped(src, 1024); err == nil {
		t.Fatal("oversize source admitted")
	}
	if src.Len() == 0 {
		t.Fatal("capped read consumed the entire oversize source")
	}
}

func TestZstdStreamHidesByter(t *testing.T) {
	var r io.Reader = zstdStream{bytes.NewReader(nil)}
	if _, ok := r.(interface{ Bytes() []byte }); ok {
		t.Fatal("zstd stream reader exposes Bytes; klauspost would DecodeAll before the cap")
	}
	if _, ok := r.(interface{ Len() int }); ok {
		t.Fatal("zstd stream reader exposes Len; klauspost would DecodeAll before the cap")
	}
}

func TestEncodeRefusesInvalidMembers(t *testing.T) {
	if _, err := Encode("", Archive{Config: []byte("listen: 8080\n")}); err == nil {
		t.Fatal("Encode returned bytes for a config Validate would reject")
	}
	if _, err := Encode(t.TempDir(), Archive{Database: []byte("SQLite format 3\x00not-a-db")}); err == nil {
		t.Fatal("Encode returned bytes for a database CheckDatabase would reject")
	}
}

// TestEncodeFileMatchesFormatByteForByte rebuilds the version-1 archive with
// the pre-streaming layout (one zstd Write over the complete payload) and
// requires both the file path and the byte-buffered wrapper to emit exactly
// those bytes for config-only, database-only, and both members.
func TestEncodeFileMatchesFormatByteForByte(t *testing.T) {
	cfg := defaultConfigYAML(t)
	db := packedRequestsDB(t)
	created := time.Unix(1_700_000_123, 0).UTC()
	cases := []struct {
		name string
		a    Archive
	}{
		{"config-only", Archive{Created: created, Config: cfg}},
		{"database-only", Archive{Created: created, Database: db}},
		{"both", Archive{Created: created, Config: cfg, Database: db}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := referenceArchive(t, tc.a)
			stage := t.TempDir()
			path, size, err := EncodeFile(stage, tc.a, int64(config.BackupMaxBytesMax))
			if err != nil {
				t.Fatal(err)
			}
			if path == "" {
				t.Fatal("EncodeFile returned an empty path")
			}
			if size != int64(len(want)) {
				t.Fatalf("EncodeFile size = %d, want %d", size, len(want))
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, want) {
				t.Fatalf("EncodeFile bytes differ from the version-1 layout at byte %d", firstDiff(raw, want))
			}
			buffered, err := Encode(stage, tc.a)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buffered, want) {
				t.Fatalf("Encode bytes differ from the version-1 layout at byte %d", firstDiff(buffered, want))
			}
			assertNoArchiveStaging(t, stage)
		})
	}
}

// TestEncodeFilePathSourceMatchesBytes pins the file-backed database source:
// for the same packed SQLite member and Created time, the path-backed archive
// is byte-for-byte identical to the byte-backed archive and to the frozen
// version-1 layout, for config-only, database-only and both-member archives.
// Validate also accepts the path without a staging directory because it
// integrity-checks the private file in place.
func TestEncodeFilePathSourceMatchesBytes(t *testing.T) {
	cfg := defaultConfigYAML(t)
	db := packedRequestsDB(t)
	created := time.Unix(1_700_000_123, 0).UTC()
	dbPath := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(dbPath, db, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name             string
		config, database bool
	}{
		{"config-only", true, false},
		{"database-only", false, true},
		{"both", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			byteA := Archive{Created: created}
			pathA := Archive{Created: created}
			if tc.config {
				byteA.Config = cfg
				pathA.Config = cfg
			}
			if tc.database {
				byteA.Database = db
				pathA.DatabasePath = dbPath
			}
			want := referenceArchive(t, byteA)
			stage := t.TempDir()

			byteRaw, err := Encode(stage, byteA)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(byteRaw, want) {
				t.Fatalf("byte-backed encode differs from the version-1 layout at byte %d", firstDiff(byteRaw, want))
			}

			path, size, err := EncodeFile(stage, pathA, int64(config.BackupMaxBytesMax))
			if err != nil {
				t.Fatal(err)
			}
			if path == "" || size != int64(len(want)) {
				t.Fatalf("EncodeFile returned path=%q size=%d, want size %d", path, size, len(want))
			}
			pathRaw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(pathRaw, want) {
				t.Fatalf("path-backed archive differs from the version-1 layout at byte %d", firstDiff(pathRaw, want))
			}
			if !bytes.Equal(pathRaw, byteRaw) {
				t.Fatalf("path-backed and byte-backed archives differ at byte %d", firstDiff(pathRaw, byteRaw))
			}

			bufferedPath, err := Encode(stage, pathA)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(bufferedPath, want) {
				t.Fatalf("path-backed Encode differs from the version-1 layout at byte %d", firstDiff(bufferedPath, want))
			}
			if err := Validate("", pathA); err != nil {
				t.Fatalf("path-backed Validate without a staging directory: %v", err)
			}
			assertNoArchiveStaging(t, stage)
		})
	}

	if _, err := Encode(t.TempDir(), Archive{Database: db, DatabasePath: dbPath}); err == nil {
		t.Fatal("archive with both database sources was encoded")
	}
	if err := Validate(t.TempDir(), Archive{Database: db, DatabasePath: dbPath}); err == nil {
		t.Fatal("archive with both database sources was validated")
	}
	bad := filepath.Join(t.TempDir(), "bad.db")
	if err := os.WriteFile(bad, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Validate("", Archive{DatabasePath: bad}); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("invalid path-backed database = %v, want ErrInvalidSnapshot", err)
	}
}

// TestEncodeFilePathSourceSelfCheckRejectsCorruption pins the streaming
// self-check for a file-backed database member: a byte change, truncation, or
// growth in the source after encoding fails before Validate, and no archive
// is mistaken for the changed snapshot.
func TestEncodeFilePathSourceSelfCheckRejectsCorruption(t *testing.T) {
	db := packedRequestsDB(t)
	src := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(src, db, 0o600); err != nil {
		t.Fatal(err)
	}
	a := Archive{Created: time.Unix(1_700_000_123, 0).UTC(), DatabasePath: src}
	stage := t.TempDir()
	path, _, err := EncodeFile(stage, a, int64(config.BackupMaxBytesMax))
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	flipped := append([]byte(nil), db...)
	flipped[len(flipped)/2] ^= 0xff
	if err := os.WriteFile(src, flipped, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := selfCheckFile(path, stage, a); err == nil || !strings.Contains(err.Error(), "payload changed") {
		t.Fatalf("source byte corruption self-check = %v, want payload changed", err)
	}

	if err := os.WriteFile(src, db[:len(db)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := selfCheckFile(path, stage, a); err == nil || !strings.Contains(err.Error(), "payload changed") {
		t.Fatalf("source truncation self-check = %v, want payload changed", err)
	}

	if err := os.WriteFile(src, append(append([]byte(nil), db...), 0), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := selfCheckFile(path, stage, a); err == nil || !strings.Contains(err.Error(), "payload changed") {
		t.Fatalf("source growth self-check = %v, want payload changed", err)
	}
}

// TestEncodeFileFailsClosedOnDatabaseSizeChange proves the streaming encoder
// never frames a file that changed after it was sized: a deterministic shrink
// or growth during the member write fails closed, returns no artifact, and
// leaves no staging file. A missing source also fails before encoding.
func TestEncodeFileFailsClosedOnDatabaseSizeChange(t *testing.T) {
	base := packedRequestsDB(t)
	cases := []struct {
		name   string
		mutate func(t *testing.T, path string)
	}{
		{"shrank", func(t *testing.T, path string) {
			if err := os.Truncate(path, int64(len(base))-1); err != nil {
				t.Fatal(err)
			}
		}},
		{"grew", func(t *testing.T, path string) {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.Write([]byte{0}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "snapshot.db")
			if err := os.WriteFile(src, base, 0o600); err != nil {
				t.Fatal(err)
			}
			old := databaseStreamHook
			databaseStreamHook = func(path string) {
				databaseStreamHook = old
				tc.mutate(t, path)
			}
			t.Cleanup(func() { databaseStreamHook = old })

			stage := t.TempDir()
			path, size, err := EncodeFile(stage, Archive{Created: time.Unix(1_700_000_123, 0).UTC(), DatabasePath: src}, int64(config.BackupMaxBytesMax))
			if err == nil || !strings.Contains(err.Error(), "changed size") {
				t.Fatalf("source %s during encode = %v, want a changed-size failure", tc.name, err)
			}
			if path != "" || size != 0 {
				t.Fatalf("failed encode returned path=%q size=%d", path, size)
			}
			assertNoArchiveStaging(t, stage)
		})
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, _, err := EncodeFile(t.TempDir(), Archive{DatabasePath: missing}, int64(config.BackupMaxBytesMax)); err == nil {
		t.Fatal("encode accepted a missing database source")
	}
}

// TestEncodeFileSelfCheckRejectsCorruption pins the streaming self-check: a
// byte change in the body or in the outer hash must fail before the file can
// be returned.
func TestEncodeFileSelfCheckRejectsCorruption(t *testing.T) {
	a := Archive{Created: time.Unix(1_700_000_123, 0).UTC(), Config: defaultConfigYAML(t)}
	dir := t.TempDir()
	path, _, err := EncodeFile(dir, a, int64(config.BackupMaxBytesMax))
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	hashFlipped := append([]byte(nil), raw...)
	hashFlipped[16] ^= 0xff
	if err := os.WriteFile(path, hashFlipped, 0o600); err != nil {
		t.Fatal(err)
	}
	err = selfCheckFile(path, "", a)
	if err == nil || !strings.Contains(err.Error(), "payload hash mismatch") {
		t.Fatalf("outer hash corruption self-check = %v, want payload hash mismatch", err)
	}

	bodyFlipped := append([]byte(nil), raw...)
	bodyFlipped[len(bodyFlipped)-1] ^= 0xff
	if err := os.WriteFile(path, bodyFlipped, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := selfCheckFile(path, "", a); err == nil {
		t.Fatal("body corruption passed the streaming self-check")
	}
}

// TestEncodeFileCleansStagingOnFailure pins both failure cleanup owners: the
// archive temp file written before Validate and the staging directory used by
// the database inspector.
func TestEncodeFileCleansStagingOnFailure(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := EncodeFile(dir, Archive{Config: []byte("listen: 8080\n")}, int64(config.BackupMaxBytesMax)); err == nil {
		t.Fatal("invalid config encoded")
	}
	assertNoArchiveStaging(t, dir)
	if _, _, err := EncodeFile(dir, Archive{Database: []byte("SQLite format 3\x00not-a-db")}, int64(config.BackupMaxBytesMax)); err == nil {
		t.Fatal("invalid database encoded")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("failed encode left staging entries: %v", ents)
	}
}

// TestEncodeFilePayloadCapPathSource pins the uncompressed payload ceiling for
// a file-backed member: an oversized packed database is rejected before an
// archive staging file exists, and no partial artifact is returned.
func TestEncodeFilePayloadCapPathSource(t *testing.T) {
	db := packedRequestsDBBlob(t, 2<<10)
	src := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(src, db, 0o600); err != nil {
		t.Fatal(err)
	}
	old := payloadMax
	payloadMax = 1024
	t.Cleanup(func() { payloadMax = old })

	stage := t.TempDir()
	path, size, err := EncodeFile(stage, Archive{Created: time.Unix(1_700_000_123, 0).UTC(), DatabasePath: src}, int64(config.BackupMaxBytesMax))
	if err == nil || !strings.Contains(err.Error(), "backup_max_bytes maximum") {
		t.Fatalf("path source over the payload cap = %v, want the payload cap error", err)
	}
	if path != "" || size != 0 {
		t.Fatalf("over-cap encode returned path=%q size=%d", path, size)
	}
	assertNoArchiveStaging(t, stage)
}

// TestEncodeFileEnforcesCapBeforeArtifact proves the configured archive
// ceiling stops the stream, removes the partial file, and returns no path.
func TestEncodeFileEnforcesCapBeforeArtifact(t *testing.T) {
	dir := t.TempDir()
	// ~2 MiB of high-entropy rows makes the encoded archive exceed the
	// Schema minimum 1 MiB ceiling even after zstd.
	db := packedRequestsDBBlob(t, 2<<20)
	path, size, err := EncodeFile(dir, Archive{Created: time.Unix(1_700_000_123, 0).UTC(), Database: db}, int64(config.BackupMaxBytesMin))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("cap enforcement error = %v, want ErrTooLarge", err)
	}
	if path != "" || size != 0 {
		t.Fatalf("cap enforcement returned path=%q size=%d", path, size)
	}
	assertNoArchiveStaging(t, dir)
}

// TestEncodeFileConcurrentStagingUniqueAndClean runs bounded concurrent
// encodes into one staging directory: every file name is distinct and every
// successful and failed path is removed by its owner.
func TestEncodeFileConcurrentStagingUniqueAndClean(t *testing.T) {
	stage := t.TempDir()
	a := Archive{Created: time.Unix(1_700_000_123, 0).UTC(), Config: defaultConfigYAML(t)}
	const workers = 8
	paths := make([]string, workers)
	errs := make([]error, workers)
	sizes := make([]int64, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths[i], sizes[i], errs[i] = EncodeFile(stage, a, int64(config.BackupMaxBytesMax))
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("encode %d: %v", i, errs[i])
		}
		if paths[i] == "" || sizes[i] <= 0 {
			t.Fatalf("encode %d returned path=%q size=%d", i, paths[i], sizes[i])
		}
		if seen[paths[i]] {
			t.Fatalf("concurrent encodes shared staging path %q", paths[i])
		}
		seen[paths[i]] = true
		if _, err := os.Stat(paths[i]); err != nil {
			t.Fatalf("encode %d artifact missing: %v", i, err)
		}
	}
	for path := range seen {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	assertNoArchiveStaging(t, stage)
	// A failing concurrent-class encode must also leave no artifact.
	if _, _, err := EncodeFile(stage, Archive{Config: []byte("listen: 8080\n")}, int64(config.BackupMaxBytesMax)); err == nil {
		t.Fatal("invalid config encoded")
	}
	assertNoArchiveStaging(t, stage)
}

// BenchmarkEncodeFile measures the streaming owner without a retained final
// archive. BenchmarkEncode measures the legacy byte-buffered wrapper, whose
// ReadFile adds one full archive copy; the -benchmem difference is the copy
// the handler no longer takes.
func BenchmarkEncodeFile(b *testing.B) {
	benchEncode(b, false)
}

func BenchmarkEncode(b *testing.B) {
	benchEncode(b, true)
}

func benchEncode(b *testing.B, buffered bool) {
	b.Helper()
	cfg := largeValidConfig(b, 2<<20)
	a := Archive{Created: time.Unix(1_700_000_123, 0).UTC(), Config: cfg}
	b.SetBytes(int64(len(cfg)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if buffered {
			if _, err := Encode(b.TempDir(), a); err != nil {
				b.Fatal(err)
			}
			continue
		}
		path, _, err := EncodeFile(b.TempDir(), a, int64(config.BackupMaxBytesMax))
		if err != nil {
			b.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncodeFileDatabaseSource compares the file-backed database member
// with the byte-backed source. The bytes sub-benchmark performs the legacy
// whole-database os.ReadFile before each encode, so its B/op includes the
// packed-database heap copy this change removes; the path sub-benchmark never
// materializes it. Both write and self-check the same archive, so what remains
// allocated in either case is zstd's encoder/decoder state (SpeedBestCompression
// dominates the absolute B/op) plus the 32 KiB streaming chunks, not the
// database member.
func BenchmarkEncodeFileDatabaseSource(b *testing.B) {
	dbPath, size := packedRequestsDBFile(b, 4<<20)
	created := time.Unix(1_700_000_123, 0).UTC()
	limit := int64(config.BackupMaxBytesMax)

	b.Run("bytes", func(b *testing.B) {
		stage := b.TempDir()
		b.SetBytes(size)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			data, err := os.ReadFile(dbPath)
			if err != nil {
				b.Fatal(err)
			}
			path, _, err := EncodeFile(stage, Archive{Created: created, Database: data}, limit)
			if err != nil {
				b.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("path", func(b *testing.B) {
		stage := b.TempDir()
		a := Archive{Created: created, DatabasePath: dbPath}
		b.SetBytes(size)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			path, _, err := EncodeFile(stage, a, limit)
			if err != nil {
				b.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func defaultConfigYAML(t testing.TB) []byte {
	t.Helper()
	var yaml bytes.Buffer
	if err := config.WriteYAML(&yaml, config.Default()); err != nil {
		t.Fatal(err)
	}
	return yaml.Bytes()
}

// referenceArchive is the frozen version-1 layout: member framing, outer
// SHA-256, one zstd Write over the whole payload, and the fixed header.
func referenceArchive(t *testing.T, a Archive) []byte {
	t.Helper()
	var payload bytes.Buffer
	write := func(kind uint8, data []byte) {
		payload.WriteByte(kind)
		payload.Write(binary.BigEndian.AppendUint64(nil, uint64(len(data))))
		payload.Write(data)
	}
	if len(a.Config) > 0 {
		write(memberConfig, a.Config)
	}
	if len(a.Database) > 0 {
		write(memberDatabase, a.Database)
	}
	sum := sha256.Sum256(payload.Bytes())
	var body bytes.Buffer
	enc, err := zstd.NewWriter(&body,
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithEncoderCRC(true),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(payload.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	raw := append([]byte(magic), archiveVersion, a.flags(), 0, 0)
	raw = binary.BigEndian.AppendUint64(raw, uint64(a.Created.Unix()))
	raw = append(raw, sum[:]...)
	return append(raw, body.Bytes()...)
}

func firstDiff(got, want []byte) int {
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			return i
		}
	}
	return n
}

func assertNoArchiveStaging(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), "millivolt-backup-") && strings.HasSuffix(e.Name(), ".mvb") {
			t.Fatalf("staging file left behind: %s", e.Name())
		}
	}
}

// largeValidConfig builds a YAML document whose comments carry enough
// entropy that zstd cannot shrink the archive under the test's ceiling.
func largeValidConfig(t testing.TB, minBytes int) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("# generated backup fixture\n")
	sum := sha256.Sum256([]byte("backup-cap"))
	for b.Len() < minBytes {
		sum = sha256.Sum256(sum[:])
		fmt.Fprintf(&b, "# %x\n", sum[:16])
	}
	return b.Bytes()
}

func packedRequestsDBBlob(t *testing.T, blobBytes int) []byte {
	t.Helper()
	path, _ := packedRequestsDBFile(t, blobBytes)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// packedRequestsDBFile leaves the packed fixture on disk and returns its path
// and exact size, which the file-backed source benchmark needs without holding
// the database in heap.
func packedRequestsDBFile(tb testing.TB, blobBytes int) (string, int64) {
	tb.Helper()
	dir := tb.TempDir()
	path := filepath.Join(dir, "src.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE requests (id TEXT PRIMARY KEY, payload BLOB)`); err != nil {
		tb.Fatal(err)
	}
	if blobBytes > 0 {
		blob := largeValidConfig(tb, blobBytes)
		if _, err := db.Exec(`INSERT INTO requests(id, payload) VALUES ('big', ?)`, blob); err != nil {
			tb.Fatal(err)
		}
	} else if _, err := db.Exec(`INSERT INTO requests(id) VALUES ('rec-1')`); err != nil {
		tb.Fatal(err)
	}
	snap := filepath.Join(dir, "snap.db")
	if _, err := db.Exec("VACUUM INTO ?", snap); err != nil {
		tb.Fatal(err)
	}
	db.Close()
	info, err := os.Stat(snap)
	if err != nil {
		tb.Fatal(err)
	}
	return snap, info.Size()
}

func packedRequestsDB(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE requests (id TEXT PRIMARY KEY); INSERT INTO requests(id) VALUES ('rec-1')`); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(t.TempDir(), "snap.db")
	if _, err := db.Exec("VACUUM INTO ?", snap); err != nil {
		t.Fatal(err)
	}
	db.Close()
	data, err := os.ReadFile(snap)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
