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

// TestDecodeFileMatchesDecode pins the one decoder core: for config-only,
// database-only and both-member archives with a fixed Created time, the
// file-backed decoder yields the same Created, config bytes and database
// bytes as the byte-backed decoder. The decoded database stays in a private
// directory the caller removes.
func TestDecodeFileMatchesDecode(t *testing.T) {
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
			source := t.TempDir()
			path, _, err := EncodeFile(source, tc.a, int64(config.BackupMaxBytesMax))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want, err := Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			stage := t.TempDir()
			got, err := DecodeFile(stage, path)
			if err != nil {
				t.Fatal(err)
			}
			if got.Created.Unix() != want.Created.Unix() {
				t.Fatalf("created = %s, want %s", got.Created, want.Created)
			}
			if !bytes.Equal(got.Config, want.Config) {
				t.Fatal("file-backed decode changed the config member")
			}
			if len(want.Database) == 0 {
				if got.DatabasePath != "" {
					t.Fatalf("config-only archive decoded a database file %q", got.DatabasePath)
				}
			} else {
				if got.DatabasePath == "" {
					t.Fatal("database member was not decoded to a file")
				}
				data, err := os.ReadFile(got.DatabasePath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(data, want.Database) {
					t.Fatal("file-backed decode changed the database member")
				}
				size, err := got.DatabaseMemberSize()
				if err != nil {
					t.Fatal(err)
				}
				if size != int64(len(want.Database)) {
					t.Fatalf("database member size = %d, want %d", size, len(want.Database))
				}
			}
			if err := Validate("", got); err != nil {
				t.Fatalf("file-backed archive failed Validate: %v", err)
			}
			if got.DatabasePath != "" {
				if err := os.RemoveAll(filepath.Dir(got.DatabasePath)); err != nil {
					t.Fatal(err)
				}
			}
			assertNoRestoreStaging(t, stage)
		})
	}
}

// TestDecodeFileCleansStagingOnRejection pins fail-closed file decoding: a
// truncated, corrupted, trailing-payload, missing-member or malformed archive
// returns no Archive and leaves none of its decoded database staging behind.
func TestDecodeFileCleansStagingOnRejection(t *testing.T) {
	cfg := defaultConfigYAML(t)
	created := time.Unix(1_700_000_123, 0).UTC()
	valid, err := Encode(t.TempDir(), Archive{Created: created, Config: cfg, Database: packedRequestsDB(t)})
	if err != nil {
		t.Fatal(err)
	}
	configPayload := framedMember(memberConfig, cfg)
	cases := []struct {
		name string
		raw  []byte
	}{
		{"truncated", append([]byte(nil), valid[:len(valid)-8]...)},
		{"corrupted", func() []byte {
			raw := append([]byte(nil), valid...)
			raw[len(raw)/2] ^= 0xff
			return raw
		}()},
		{"outer-hash", func() []byte {
			raw := append([]byte(nil), valid...)
			raw[16] ^= 0xff
			return raw
		}()},
		{"garbage", []byte("not a backup")},
		{"trailing-payload", framedArchive(t, flagConfig, created, append(append([]byte(nil), configPayload...), 1, 2, 3))},
		{"missing-database", framedArchive(t, flagDatabase, created, nil)},
		{"missing-config", framedArchive(t, flagConfig, created, nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "backup.mvb")
			if err := os.WriteFile(path, tc.raw, 0o600); err != nil {
				t.Fatal(err)
			}
			stage := t.TempDir()
			got, err := DecodeFile(stage, path)
			if err == nil {
				t.Fatal("invalid archive decoded")
			}
			if got.Config != nil || got.Database != nil || got.DatabasePath != "" {
				t.Fatalf("failed decode returned a partial archive: %+v", got)
			}
			if _, err := Decode(tc.raw); err == nil {
				t.Fatal("byte-backed decoder admitted an archive the file-backed decoder rejected")
			}
			assertNoRestoreStaging(t, stage)
		})
	}
}

// TestDecodeFileOverCapCleansStaging pins the uncompressed payload ceiling for
// the file-backed decoder: an archive over the cap is rejected and its
// database staging directory is removed.
func TestDecodeFileOverCapCleansStaging(t *testing.T) {
	raw, err := Encode(t.TempDir(), Archive{
		Created:  time.Unix(1_700_000_123, 0).UTC(),
		Database: packedRequestsDBBlob(t, 2<<10),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "backup.mvb")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	old := payloadMax
	payloadMax = 1024
	t.Cleanup(func() { payloadMax = old })
	stage := t.TempDir()
	if _, err := DecodeFile(stage, path); err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("over-cap archive = %v, want the payload cap error", err)
	}
	assertNoRestoreStaging(t, stage)
}

// TestDecodeFileRequiresDatabaseStaging pins the file decoder's capacity
// owner: a database member cannot be decoded without the live database's
// staging volume, while a config-only archive needs none.
func TestDecodeFileRequiresDatabaseStaging(t *testing.T) {
	created := time.Unix(1_700_000_123, 0).UTC()
	dbRaw, err := Encode(t.TempDir(), Archive{Created: created, Database: packedRequestsDB(t)})
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "db.mvb")
	if err := os.WriteFile(dbPath, dbRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeFile("", dbPath); err == nil {
		t.Fatal("database archive decoded without a staging directory")
	}
	cfgRaw, err := Encode("", Archive{Created: created, Config: defaultConfigYAML(t)})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.mvb")
	if err := os.WriteFile(cfgPath, cfgRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeFile("", cfgPath)
	if err != nil {
		t.Fatalf("config-only archive without a staging directory: %v", err)
	}
	if got.DatabasePath != "" || len(got.Config) == 0 {
		t.Fatalf("config-only archive members = %+v", got)
	}
}

// TestDecodeFileConcurrentStagingUniqueAndClean pins the wildcard staging name
// under concurrent decodes: every decoded database has a distinct directory
// and the caller-owned cleanup removes each one.
func TestDecodeFileConcurrentStagingUniqueAndClean(t *testing.T) {
	raw, err := Encode(t.TempDir(), Archive{
		Created:  time.Unix(1_700_000_123, 0).UTC(),
		Config:   defaultConfigYAML(t),
		Database: packedRequestsDB(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "backup.mvb")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	const workers = 8
	paths := make([]string, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := DecodeFile(stage, path)
			if err != nil {
				errs[i] = err
				return
			}
			paths[i] = a.DatabasePath
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		dir := filepath.Dir(paths[i])
		if seen[dir] {
			t.Fatalf("concurrent decodes shared staging directory %q", dir)
		}
		seen[dir] = true
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	assertNoRestoreStaging(t, stage)
}

// BenchmarkDecodeFileDatabaseSource compares the legacy byte-backed restore
// path (upload into heap + Decode's full payload + a database member copy)
// with the file-backed decoder across two packed-database sizes. The file path
// never materializes the database member on the heap, so its B/op stays
// bounded by zstd decoder state and the fixed streaming buffer instead of
// tracking the member size.
func BenchmarkDecodeFileDatabaseSource(b *testing.B) {
	for _, blobBytes := range []int{2 << 20, 8 << 20} {
		b.Run(fmt.Sprintf("%dMiB", blobBytes>>20), func(b *testing.B) {
			benchDecodeFileDatabaseSource(b, blobBytes)
		})
	}
}

func benchDecodeFileDatabaseSource(b *testing.B, blobBytes int) {
	dbPath, size := packedRequestsDBFile(b, blobBytes)
	data, err := os.ReadFile(dbPath)
	if err != nil {
		b.Fatal(err)
	}
	archivePath, _, err := EncodeFile(b.TempDir(), Archive{
		Created:  time.Unix(1_700_000_123, 0).UTC(),
		Database: data,
	}, int64(config.BackupMaxBytesMax))
	if err != nil {
		b.Fatal(err)
	}

	b.Run("bytes", func(b *testing.B) {
		b.SetBytes(size)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			upload, err := os.ReadFile(archivePath)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := Decode(upload); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("file", func(b *testing.B) {
		stage := b.TempDir()
		b.SetBytes(size)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			a, err := DecodeFile(stage, archivePath)
			if err != nil {
				b.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Dir(a.DatabasePath)); err != nil {
				b.Fatal(err)
			}
		}
	})
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

// TestValidateSelectedFailsClosedOnUndecodedDatabase pins the exported
// selection contract: a stream-discarded member is present and sized but has
// no bytes or path, so asking for database validation must fail instead of
// returning success without checking anything. An unselected member stays
// deliberately unvalidated.
func TestValidateSelectedFailsClosedOnUndecodedDatabase(t *testing.T) {
	discarded := Archive{DatabasePresent: true, DatabaseBytes: 4096}
	if err := ValidateSelected("", discarded, true); err == nil {
		t.Fatal("ValidateSelected(true) accepted an undecoded database member")
	}
	if err := ValidateSelected("", discarded, false); err != nil {
		t.Fatalf("ValidateSelected(false) = %v, want the unselected member ignored", err)
	}
	var yaml bytes.Buffer
	if err := config.WriteYAML(&yaml, config.Default()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSelected("", Archive{Config: yaml.Bytes(), DatabasePresent: true}, true); err == nil {
		t.Fatal("config-plus-undecoded-database validation succeeded")
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
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckDatabaseFile(empty); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("empty file: %v, want ErrInvalidSnapshot", err)
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

// TestConfigMemberCapIsDocumented16MiB pins the documented config-member bound
// independently of config.ConfigDocumentMaxBytes, so a one-sided constant
// change cannot keep the contract green while the documented 16 MiB drifts.
func TestConfigMemberCapIsDocumented16MiB(t *testing.T) {
	const documentedConfigMemberMax = 16 << 20
	if configMemberMax != documentedConfigMemberMax {
		t.Fatalf("configMemberMax = %d, want the documented 16 MiB (%d)", configMemberMax, documentedConfigMemberMax)
	}
	if config.ConfigDocumentMaxBytes != documentedConfigMemberMax {
		t.Fatalf("config.ConfigDocumentMaxBytes = %d, want the documented 16 MiB (%d)", config.ConfigDocumentMaxBytes, documentedConfigMemberMax)
	}
}

// TestEncodeConfigMemberCap pins the config retention guardrail on the encode
// side: exactly the cap is accepted and round-trips, while one byte more is
// refused before any staging artifact exists. The cap applies only to the
// config member, so these fixtures never approach the archive payload ceiling.
func TestEncodeConfigMemberCap(t *testing.T) {
	created := time.Unix(1_700_000_123, 0).UTC()
	atCap := configYAMLOfSize(t, configMemberMax)
	overCap := configYAMLOfSize(t, configMemberMax+1)
	limit := int64(config.BackupMaxBytesMax)

	stage := t.TempDir()
	path, size, err := EncodeFile(stage, Archive{Created: created, Config: atCap}, limit)
	if err != nil {
		t.Fatalf("EncodeFile at the config cap: %v", err)
	}
	if path == "" || size <= 0 {
		t.Fatalf("EncodeFile at the config cap returned path=%q size=%d", path, size)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode at the config cap: %v", err)
	}
	if !bytes.Equal(got.Config, atCap) {
		t.Fatal("config at the cap changed on round-trip")
	}
	if err := Validate("", got); err != nil {
		t.Fatalf("config at the cap failed Validate: %v", err)
	}
	buffered, err := Encode(stage, Archive{Created: created, Config: atCap})
	if err != nil {
		t.Fatalf("Encode at the config cap: %v", err)
	}
	if !bytes.Equal(buffered, raw) {
		t.Fatal("Encode and EncodeFile disagree at the config cap")
	}
	assertNoArchiveStaging(t, stage)

	reject := func(t *testing.T, encode func(dir string, a Archive) (string, error)) {
		t.Helper()
		stage := t.TempDir()
		path, err := encode(stage, Archive{Created: created, Config: overCap})
		if err == nil || !strings.Contains(err.Error(), "config member exceeds") {
			t.Fatalf("over-cap config encode = %v, want the config cap error", err)
		}
		if path != "" {
			t.Fatalf("over-cap config encode returned path %q", path)
		}
		ents, err := os.ReadDir(stage)
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) != 0 {
			t.Fatalf("over-cap config encode left staging entries: %v", ents)
		}
	}
	t.Run("EncodeFile", func(t *testing.T) {
		reject(t, func(dir string, a Archive) (string, error) {
			path, size, err := EncodeFile(dir, a, limit)
			if path != "" || size != 0 {
				t.Fatalf("rejected EncodeFile returned path=%q size=%d", path, size)
			}
			return path, err
		})
	})
	t.Run("Encode", func(t *testing.T) {
		reject(t, func(dir string, a Archive) (string, error) {
			_, err := Encode(dir, a)
			return "", err
		})
	})
}

// TestDecodeConfigMemberCap pins the shared byte decoder: an over-cap config
// member is rejected with the cap error, and a declared length alone is
// rejected before the decoder copies a body that is not there.
func TestDecodeConfigMemberCap(t *testing.T) {
	created := time.Unix(1_700_000_123, 0).UTC()
	overCap := configYAMLOfSize(t, configMemberMax+1)

	raw := framedArchive(t, flagConfig, created, framedMember(memberConfig, overCap))
	got, err := Decode(raw)
	if err == nil || !strings.Contains(err.Error(), "config member exceeds") {
		t.Fatalf("over-cap config decode = %v, want the config cap error", err)
	}
	if got.Config != nil {
		t.Fatalf("over-cap config decode retained %d config bytes", len(got.Config))
	}

	// The declared length is rejected before the body is read: this archive
	// has no member body at all, so a decoder that copied first would report
	// truncation instead of the cap.
	declared := binary.BigEndian.AppendUint64([]byte{memberConfig}, uint64(configMemberMax+1))
	forged := framedArchive(t, flagConfig, created, declared)
	if _, err := Decode(forged); err == nil || !strings.Contains(err.Error(), "config member exceeds") {
		t.Fatalf("declared over-cap config decode = %v, want the config cap error before the body", err)
	}
}

// TestDecodeFileConfigMemberCap pins the file decoder's shared cap: it refuses
// an over-cap config member before buffering it and removes any database
// staging it already created.
func TestDecodeFileConfigMemberCap(t *testing.T) {
	created := time.Unix(1_700_000_123, 0).UTC()
	cfgPayload := framedMember(memberConfig, configYAMLOfSize(t, configMemberMax+1))
	dbPayload := framedMember(memberDatabase, packedRequestsDB(t))
	cases := []struct {
		name    string
		flags   byte
		payload []byte
	}{
		{"config-only", flagConfig, append([]byte(nil), cfgPayload...)},
		{"database-first", flagConfig | flagDatabase, append(append([]byte(nil), dbPayload...), cfgPayload...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := framedArchive(t, tc.flags, created, tc.payload)
			path := filepath.Join(t.TempDir(), "backup.mvb")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			stage := t.TempDir()
			got, err := DecodeFile(stage, path)
			if got.DatabasePath != "" {
				if err := os.RemoveAll(filepath.Dir(got.DatabasePath)); err != nil {
					t.Fatal(err)
				}
			}
			if err == nil || !strings.Contains(err.Error(), "config member exceeds") {
				t.Fatalf("over-cap config file decode = %v, want the config cap error", err)
			}
			if got.Config != nil || got.DatabasePath != "" {
				t.Fatalf("failed decode returned a partial archive: %+v", got)
			}
			ents, err := os.ReadDir(stage)
			if err != nil {
				t.Fatal(err)
			}
			if len(ents) != 0 {
				t.Fatalf("over-cap config file decode left staging: %v", ents)
			}
		})
	}
}

// TestDatabaseMemberAboveConfigCapDecodes proves the cap is config-only: a
// database member larger than configMemberMax still decodes and stages because
// it stays under the archive payload ceiling.
func TestDatabaseMemberAboveConfigCapDecodes(t *testing.T) {
	dbPath, dbSize := packedRequestsDBFile(t, configMemberMax+1)
	if dbSize <= int64(configMemberMax) {
		t.Fatalf("database fixture size = %d, want above the %d-byte config cap", dbSize, configMemberMax)
	}
	created := time.Unix(1_700_000_123, 0).UTC()
	stage := t.TempDir()
	archivePath, _, err := EncodeFile(stage, Archive{Created: created, DatabasePath: dbPath}, int64(config.BackupMaxBytesMax))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatalf("database member above the config cap: %v", err)
	}
	if int64(len(got.Database)) != dbSize {
		t.Fatalf("decoded database size = %d, want %d", len(got.Database), dbSize)
	}
	restoreStage := t.TempDir()
	fileGot, err := DecodeFile(restoreStage, archivePath)
	if err != nil {
		t.Fatalf("file decode of a database member above the config cap: %v", err)
	}
	info, err := os.Stat(fileGot.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != dbSize {
		t.Fatalf("staged database size = %d, want %d", info.Size(), dbSize)
	}
	if err := os.RemoveAll(filepath.Dir(fileGot.DatabasePath)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(archivePath); err != nil {
		t.Fatal(err)
	}
	assertNoArchiveStaging(t, stage)
	assertNoRestoreStaging(t, restoreStage)
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
// allocated in either case is zstd's encoder/decoder state (the encoder level
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
		payload.Write(framedMember(kind, data))
	}
	if len(a.Config) > 0 {
		write(memberConfig, a.Config)
	}
	if len(a.Database) > 0 {
		write(memberDatabase, a.Database)
	}
	return framedArchive(t, a.flags(), a.Created, payload.Bytes())
}

// framedMember is one version-1 member: kind, big-endian length, body.
func framedMember(kind uint8, data []byte) []byte {
	out := []byte{kind}
	out = binary.BigEndian.AppendUint64(out, uint64(len(data)))
	return append(out, data...)
}

// framedArchive wraps an already-framed payload in the fixed header, its outer
// SHA-256 and one zstd frame. Tests use it for payloads the encoder never
// produces, such as trailing bytes and missing members.
func framedArchive(t *testing.T, flags byte, created time.Time, payload []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(payload)
	var body bytes.Buffer
	// The zstd configuration comes from the production owner, so this
	// reference freezes the header and member framing - the actual wire
	// contract - instead of duplicating a tuning constant.
	enc, err := newArchiveEncoder(&body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	raw := append([]byte(magic), archiveVersion, flags, 0, 0)
	raw = binary.BigEndian.AppendUint64(raw, uint64(created.Unix()))
	raw = append(raw, sum[:]...)
	return append(raw, body.Bytes()...)
}

func assertNoRestoreStaging(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "millivolt-restore-") {
			t.Fatalf("restore staging left behind: %s", e.Name())
		}
	}
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

// configYAMLOfSize builds a comment-only YAML document of exactly size bytes.
// Comments keep the cap fixtures valid for config.LoadBytes without a large
// schema body, so a test can reach the limit without multi-GiB data.
func configYAMLOfSize(t testing.TB, size int) []byte {
	t.Helper()
	if size < 3 {
		t.Fatalf("config fixture size %d is too small for a comment", size)
	}
	doc := append([]byte("# "), bytes.Repeat([]byte("x"), size-3)...)
	return append(doc, '\n')
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
