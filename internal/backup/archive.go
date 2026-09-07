// Package backup is the single owner of millivolt operator backup archives.
//
// A backup is a packed SQLite snapshot (VACUUM INTO) and/or the schema YAML,
// wrapped in a zstd frame with a content checksum and an outer SHA-256 of the
// uncompressed payload. Encode refuses to return bytes that Decode+Validate
// would reject, so a produced archive is never a silent corrupt file.
package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/LLM-4-People/millivolt/internal/config"
)

const (
	magic          = "MVB1"
	archiveVersion = 1
	flagConfig     = 1 << 0
	flagDatabase   = 1 << 1
	knownFlags     = flagConfig | flagDatabase
	headerSize     = 4 + 1 + 1 + 2 + 8 + sha256.Size

	memberConfig   uint8 = 1
	memberDatabase uint8 = 2
)

// payloadMax is the uncompressed decode ceiling: the Schema maximum for
// backup_max_bytes. Tests lower it to lock LimitReader without allocating
// that many bytes.
var payloadMax = int64(config.BackupMaxBytesMax)

// Archive is one operator backup. At least one member must be present.
type Archive struct {
	Created  time.Time
	Config   []byte
	Database []byte
}

func (a Archive) flags() byte {
	var f byte
	if len(a.Config) > 0 {
		f |= flagConfig
	}
	if len(a.Database) > 0 {
		f |= flagDatabase
	}
	return f
}

// Encode writes a self-checked archive. The returned bytes have already
// round-tripped through Decode and Validate.
func Encode(a Archive) ([]byte, error) {
	if a.flags() == 0 {
		return nil, fmt.Errorf("backup must include config, database, or both")
	}
	if a.Created.IsZero() {
		a.Created = time.Now().UTC()
	}
	payload, err := marshalMembers(a)
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > int64(config.BackupMaxBytesMax) {
		return nil, fmt.Errorf("backup payload exceeds backup_max_bytes maximum")
	}
	sum := sha256.Sum256(payload)
	var body bytes.Buffer
	enc, err := zstd.NewWriter(&body,
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithEncoderCRC(true),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return nil, fmt.Errorf("compress backup: %w", err)
	}
	if _, err := enc.Write(payload); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("compress backup: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("compress backup: %w", err)
	}
	out := make([]byte, 0, headerSize+body.Len())
	out = append(out, magic...)
	out = append(out, archiveVersion, a.flags(), 0, 0)
	out = binary.BigEndian.AppendUint64(out, uint64(a.Created.Unix()))
	out = append(out, sum[:]...)
	out = append(out, body.Bytes()...)
	got, err := Decode(out)
	if err != nil {
		return nil, fmt.Errorf("backup self-check: %w", err)
	}
	if err := Validate(got); err != nil {
		return nil, fmt.Errorf("backup self-check: %w", err)
	}
	if !bytes.Equal(got.Config, a.Config) || !bytes.Equal(got.Database, a.Database) {
		return nil, fmt.Errorf("backup self-check: payload changed")
	}
	return out, nil
}

// Decode admits an archive only when magic, version, zstd checksum, and the
// outer SHA-256 all match. Uncompressed members cannot exceed the Schema
// maximum for backup_max_bytes, so a small frame cannot expand without bound.
// It does not apply config or open SQLite.
func Decode(raw []byte) (Archive, error) {
	return decodeLimited(raw, payloadMax)
}

func decodeLimited(raw []byte, maxPayload int64) (Archive, error) {
	if maxPayload <= 0 {
		return Archive{}, fmt.Errorf("backup payload limit is invalid")
	}
	if len(raw) < headerSize {
		return Archive{}, fmt.Errorf("backup is truncated")
	}
	if string(raw[:4]) != magic {
		return Archive{}, fmt.Errorf("not a millivolt backup")
	}
	if raw[4] != archiveVersion {
		return Archive{}, fmt.Errorf("unsupported backup version %d", raw[4])
	}
	flags := raw[5]
	if flags == 0 || flags&^knownFlags != 0 {
		return Archive{}, fmt.Errorf("backup flags %02x are not a known config/database set", flags)
	}
	if raw[6] != 0 || raw[7] != 0 {
		return Archive{}, fmt.Errorf("backup header is reserved and must be zero")
	}
	created := time.Unix(int64(binary.BigEndian.Uint64(raw[8:16])), 0).UTC()
	var want [sha256.Size]byte
	copy(want[:], raw[16:headerSize])
	dec, err := zstd.NewReader(bytes.NewReader(raw[headerSize:]), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return Archive{}, fmt.Errorf("decompress backup: %w", err)
	}
	defer dec.Close()
	payload, err := readCapped(dec, maxPayload)
	if err != nil {
		return Archive{}, fmt.Errorf("decompress backup: %w", err)
	}
	if sha256.Sum256(payload) != want {
		return Archive{}, fmt.Errorf("backup payload hash mismatch")
	}
	a, err := unmarshalMembers(payload, flags)
	if err != nil {
		return Archive{}, err
	}
	a.Created = created
	return a, nil
}

func readCapped(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		return nil, fmt.Errorf("backup payload limit is invalid")
	}
	payload, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > max {
		return nil, fmt.Errorf("backup payload exceeds maximum size")
	}
	return payload, nil
}

// Validate runs semantic checks: config YAML must load without dropped keys,
// and a database member must be a packed SQLite snapshot with a requests table.
func Validate(a Archive) error {
	if a.flags() == 0 {
		return fmt.Errorf("backup must include config, database, or both")
	}
	if len(a.Config) > 0 {
		cfg, skipped, err := config.LoadBytes(a.Config)
		if err != nil {
			return fmt.Errorf("backup config: %w", err)
		}
		if len(skipped) > 0 {
			return fmt.Errorf("backup config dropped keys: %s", strings.Join(skipped, ", "))
		}
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("backup config: %w", err)
		}
	}
	if len(a.Database) > 0 {
		if err := CheckDatabase(a.Database); err != nil {
			return fmt.Errorf("backup database: %w", err)
		}
	}
	return nil
}

func marshalMembers(a Archive) ([]byte, error) {
	var buf bytes.Buffer
	write := func(kind uint8, data []byte) error {
		if err := buf.WriteByte(kind); err != nil {
			return err
		}
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(data)))
		if _, err := buf.Write(n[:]); err != nil {
			return err
		}
		_, err := buf.Write(data)
		return err
	}
	if len(a.Config) > 0 {
		if err := write(memberConfig, a.Config); err != nil {
			return nil, err
		}
	}
	if len(a.Database) > 0 {
		if err := write(memberDatabase, a.Database); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func unmarshalMembers(payload []byte, flags byte) (Archive, error) {
	var a Archive
	seen := map[uint8]bool{}
	for len(payload) > 0 {
		if len(payload) < 9 {
			return Archive{}, fmt.Errorf("backup member is truncated")
		}
		kind := payload[0]
		n := binary.BigEndian.Uint64(payload[1:9])
		payload = payload[9:]
		if uint64(len(payload)) < n {
			return Archive{}, fmt.Errorf("backup member is truncated")
		}
		data := payload[:n]
		payload = payload[n:]
		if seen[kind] {
			return Archive{}, fmt.Errorf("backup has a duplicate member")
		}
		seen[kind] = true
		switch kind {
		case memberConfig:
			if flags&flagConfig == 0 {
				return Archive{}, fmt.Errorf("backup config member is not flagged")
			}
			a.Config = append([]byte(nil), data...)
		case memberDatabase:
			if flags&flagDatabase == 0 {
				return Archive{}, fmt.Errorf("backup database member is not flagged")
			}
			a.Database = append([]byte(nil), data...)
		default:
			return Archive{}, fmt.Errorf("unknown backup member %d", kind)
		}
	}
	if flags&flagConfig != 0 && len(a.Config) == 0 {
		return Archive{}, fmt.Errorf("backup is missing its config member")
	}
	if flags&flagDatabase != 0 && len(a.Database) == 0 {
		return Archive{}, fmt.Errorf("backup is missing its database member")
	}
	return a, nil
}
