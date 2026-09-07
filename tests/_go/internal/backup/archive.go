package backup

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/LLM-4-People/millivolt/internal/config"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	var yaml bytes.Buffer
	if err := config.WriteYAML(&yaml, config.Default()); err != nil {
		t.Fatal(err)
	}
	db := packedRequestsDB(t)
	raw, err := Encode(Archive{Created: time.Unix(1_700_000_000, 0).UTC(), Config: yaml.Bytes(), Database: db})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(got); err != nil {
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
	cfgRaw, err := Encode(Archive{Config: yaml.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(cfgRaw)
	if err != nil || len(got.Database) != 0 || !bytes.Equal(got.Config, yaml.Bytes()) {
		t.Fatalf("config-only: %v len(db)=%d", err, len(got.Database))
	}
	dbRaw, err := Encode(Archive{Database: packedRequestsDB(t)})
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
	raw, err := Encode(Archive{Config: yaml.Bytes()})
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
	if _, err := Encode(Archive{}); err == nil {
		t.Fatal("empty archive encoded")
	}
}

func TestValidateRejectsInvalidMembers(t *testing.T) {
	if err := Validate(Archive{Config: []byte("listen: 8080\n")}); err == nil {
		t.Fatal("invalid config admitted")
	}
	if err := Validate(Archive{Database: []byte("SQLite format 3\x00not-a-db")}); err == nil {
		t.Fatal("invalid sqlite admitted")
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
