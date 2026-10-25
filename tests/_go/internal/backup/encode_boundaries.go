package backup

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// oneZstdBlock is the encoder's block size. A member that straddles this
// boundary is the smallest input that forces a second block, so it is where a
// level or concurrency change would first be visible in the framing.
const oneZstdBlock = 128 << 10

// sourcePayloadHash frames the archive's members exactly as EncodeFile does and
// returns the SHA-256 of the uncompressed payload. Comparing it with the
// archive header's sum proves the encoder consumed the whole framed payload and
// nothing else; comparing the decoded members with the source proves the
// decode path recovered it byte for byte.
func sourcePayloadHash(t *testing.T, a Archive) [sha256.Size]byte {
	t.Helper()
	h := sha256.New()
	p := &payloadWriter{w: io.Discard, h: h, limit: payloadMax}
	if err := writeMembers(p, a); err != nil {
		t.Fatal(err)
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// decodePayloadHash reads the archive header and returns its payload sum.
func decodePayloadHash(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := parseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	return hdr.sum
}

// TestEncodeFileRoundTripAtBlockBoundaries is the WS8 round-trip contract for
// every archive shape at the sizes where zstd's framing changes: a sub-block
// member, a member of exactly one encoder block, and one just over a block.
// EncodeFile's self-check already decodes the archive it wrote; this test then
// decodes it again through the file-backed restore owner and asserts the
// payload hash and every member survived.
func TestEncodeFileRoundTripAtBlockBoundaries(t *testing.T) {
	created := time.Unix(1_700_000_123, 0).UTC()
	limit := int64(config.BackupMaxBytesMax)
	smallDBPath, smallDBSize := packedRequestsDBFile(t, 1<<10)
	// Just over one encoder block: the smallest database that cannot be a
	// single zstd block, without paying a multi-megabyte fixture in the gate.
	bigDBPath, bigDBSize := packedRequestsDBFile(t, oneZstdBlock+16<<10)
	if smallDBSize >= oneZstdBlock {
		t.Fatalf("small database fixture is %d bytes, not sub-block", smallDBSize)
	}
	if bigDBSize <= oneZstdBlock {
		t.Fatalf("large database fixture is %d bytes, not over one block", bigDBSize)
	}

	cases := []struct {
		name string
		a    Archive
		// dbPath/dbSize are the expected database member; empty for a
		// config-only archive, which must decode no database at all.
		dbPath string
		dbSize int64
	}{
		{"config-sub-block", Archive{Created: created, Config: largeValidConfig(t, 32)}, "", 0},
		{"config-one-block", Archive{Created: created, Config: largeValidConfig(t, oneZstdBlock)}, "", 0},
		{"config-just-over-block", Archive{Created: created, Config: largeValidConfig(t, oneZstdBlock+1)}, "", 0},
		{"database-sub-block", Archive{Created: created, DatabasePath: smallDBPath}, smallDBPath, smallDBSize},
		{"database-over-block", Archive{Created: created, DatabasePath: bigDBPath}, bigDBPath, bigDBSize},
		{"combined-just-over-block", Archive{Created: created, Config: largeValidConfig(t, oneZstdBlock+1), DatabasePath: bigDBPath}, bigDBPath, bigDBSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stage := t.TempDir()
			path, size, err := EncodeFile(stage, tc.a, limit)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			defer os.Remove(path)
			raw, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if raw.Size() != size {
				t.Fatalf("returned size %d, file is %d bytes", size, raw.Size())
			}
			if got, want := decodePayloadHash(t, path), sourcePayloadHash(t, tc.a); got != want {
				t.Fatalf("payload hash changed: got %x want %x", got, want)
			}
			got, err := DecodeFile(t.TempDir(), path)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Created.Unix() != tc.a.Created.Unix() {
				t.Fatalf("created = %s", got.Created)
			}
			if !bytes.Equal(got.Config, tc.a.Config) {
				t.Fatalf("config member changed (%d vs %d bytes)", len(got.Config), len(tc.a.Config))
			}
			if tc.dbPath == "" {
				if got.DatabasePresent || got.DatabasePath != "" || len(got.Database) != 0 {
					t.Fatalf("config-only archive decoded a database member (%d bytes)", got.DatabaseBytes)
				}
				return
			}
			if !got.DatabasePresent || got.DatabaseBytes != tc.dbSize {
				t.Fatalf("database member is %d bytes, want %d", got.DatabaseBytes, tc.dbSize)
			}
			if got.DatabasePath == "" {
				t.Fatal("decoded database member has no staging path")
			}
			source, err := os.ReadFile(tc.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := os.ReadFile(got.DatabasePath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(decoded, source) {
				t.Fatal("database member changed")
			}
		})
	}
}

// TestEncodeFileDocumentedSizeFloor pins the two sizes below the smallest
// encodable member so a future change cannot treat "empty" as a valid archive:
// no members is ErrEmptyArchive, and a 1-byte config member is not a config the
// validator can accept (the self-check refuses it). Both are rejections, never
// a zero-length or partial artifact.
func TestEncodeFileDocumentedSizeFloor(t *testing.T) {
	stage := t.TempDir()
	limit := int64(config.BackupMaxBytesMax)
	for _, tc := range []struct {
		name string
		a    Archive
		want error
	}{
		{"zero-members", Archive{Created: time.Unix(1_700_000_123, 0).UTC()}, ErrEmptyArchive},
		{"one-byte-config", Archive{Created: time.Unix(1_700_000_123, 0).UTC(), Config: []byte("x")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, size, err := EncodeFile(stage, tc.a, limit)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("encode = %v, want %v", err, tc.want)
				}
			} else if err == nil {
				// The validator accepts this config, so the archive is real and
				// must decode to the same single byte.
				defer os.Remove(path)
				got, err := DecodeFile(stage, path)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				if !bytes.Equal(got.Config, tc.a.Config) {
					t.Fatalf("config member changed: %q", got.Config)
				}
				return
			}
			if size != 0 || path != "" {
				t.Fatalf("rejected encode returned path=%q size=%d", path, size)
			}
			assertNoArchiveStaging(t, stage)
		})
	}
}

// TestEncodeFileCompressedSizeWithinFactor makes a compression-level change
// accountable for archive size. Recorded on this tree before the WS8 change
// with SpeedBestCompression and concurrency 1: a 2 MiB config compresses to
// 1,070,086 bytes (0.5103 of the input) and that config plus a 4 MiB packed
// database to 2,149,734 bytes (0.3408). SpeedBetterCompression measures 0.5098
// and 0.3410, so each ceiling below is 2% above today's baseline - enough
// headroom for a zstd patch release, tight enough that SpeedDefault (0.5129 /
// 0.5130) or a weaker level fails the gate.
func TestEncodeFileCompressedSizeWithinFactor(t *testing.T) {
	created := time.Unix(1_700_000_123, 0).UTC()
	limit := int64(config.BackupMaxBytesMax)
	cfg := largeValidConfig(t, 2<<20)
	dbPath, dbSize := packedRequestsDBFile(t, 4<<20)
	cases := []struct {
		name   string
		a      Archive
		input  int64
		factor float64
	}{
		{"config-only", Archive{Created: created, Config: cfg}, int64(len(cfg)), 0.5205},
		{"combined", Archive{Created: created, Config: cfg, DatabasePath: dbPath}, int64(len(cfg)) + dbSize, 0.3476},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stage := t.TempDir()
			path, size, err := EncodeFile(stage, tc.a, limit)
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(path)
			ceiling := int64(float64(tc.input) * tc.factor)
			if size > ceiling {
				t.Fatalf("archive is %d bytes, ceiling %d (%.4f of %d input bytes)",
					size, ceiling, float64(size)/float64(tc.input), tc.input)
			}
			t.Logf("%s: %d bytes = %.4f of %d input bytes", tc.name, size, float64(size)/float64(tc.input), tc.input)
		})
	}
}

// TestEncodeFileAllocationBounded is the WS8 byte budget. The encoder level
// dominates the retained/allocated encoder state: SpeedBestCompression
// measured 77.1 MB/op and SpeedBetterCompression 44.9 MB/op for the same 2 MiB
// config, with the same 14k allocations (the decode side of the self-check).
// The 56 MB ceiling carries 20% headroom over the post-change measurement so a
// level or option change that reintroduces a heavyweight encoder fails here.
func TestEncodeFileAllocationBounded(t *testing.T) {
	created := time.Unix(1_700_000_123, 0).UTC()
	a := Archive{Created: created, Config: largeValidConfig(t, 2<<20)}
	limit := int64(config.BackupMaxBytesMax)
	stage := t.TempDir()
	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			path, _, err := EncodeFile(stage, a, limit)
			if err != nil {
				b.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				b.Fatal(err)
			}
		}
	})
	bytes, allocs := res.AllocedBytesPerOp(), res.AllocsPerOp()
	if bytes > 56<<20 {
		t.Fatalf("EncodeFile allocates %d B/op, budget %d", bytes, 56<<20)
	}
	if allocs > 17000 {
		t.Fatalf("EncodeFile allocates %d allocs/op, budget 17000", allocs)
	}
	t.Logf("EncodeFile: %d B/op %d allocs/op", bytes, allocs)
}
