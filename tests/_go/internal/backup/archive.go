package backup

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	if !strings.Contains(err.Error(), blocked) {
		t.Fatalf("error %q should name the staging directory", err)
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
