// Package backup is the single owner of millivolt operator backup archives.
//
// A backup is a packed SQLite snapshot (VACUUM INTO) and/or the schema YAML,
// wrapped in a zstd frame with a content checksum and an outer SHA-256 of the
// uncompressed payload. EncodeFile is the streaming encode owner: it writes
// one archive into a unique staging file, verifies it, and only then returns
// it. Encode is the byte-buffered wrapper for callers that need the bytes.
// Neither returns an archive that Decode+Validate would reject, so a produced
// archive is never a silent corrupt file.
package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// ErrEmptyArchive rejects an archive with no members: Encode and Validate
// both require at least one of config or database.
var ErrEmptyArchive = errors.New("backup must include config, database, or both")

// ErrTooLarge reports an encoded archive that would exceed the caller's
// backup_max_bytes ceiling. EncodeFile enforces the ceiling while streaming,
// so a partial artifact is never returned or served.
var ErrTooLarge = errors.New("backup exceeds backup_max_bytes")

const (
	magic          = "MVB1"
	archiveVersion = 1
	flagConfig     = 1 << 0
	flagDatabase   = 1 << 1
	knownFlags     = flagConfig | flagDatabase
	headerSize     = 4 + 1 + 1 + 2 + 8 + sha256.Size

	memberConfig   uint8 = 1
	memberDatabase uint8 = 2

	// decoderWindow is klauspost's default encoder window (8 MiB). Decode
	// adds it to the uncompressed cap for WithDecoderMaxMemory so a
	// streaming decode can hold that window without DecodeAll expanding a
	// small frame past payloadMax.
	decoderWindow = 8 << 20

	// archiveTempPattern names the one transient encoded artifact. The
	// wildcard keeps concurrent encodes distinct; every path removes it
	// before returning an error and the caller removes it after serving.
	archiveTempPattern = "millivolt-backup-*.mvb"

	// selfCheckBuffer bounds the comparison buffer of the streaming
	// self-check; it is reused for every member chunk.
	selfCheckBuffer = 32 << 10
)

// zstdStream is an io.Reader that does not implement Bytes or Len, so
// klauspost cannot DecodeAll a compressed body under 128KiB into an
// unbounded buffer before readCapped runs.
type zstdStream struct{ r io.Reader }

func (s zstdStream) Read(p []byte) (int, error) { return s.r.Read(p) }

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

// archiveHeader is the parsed version-1 header. createdAt has second
// precision because the wire field is Unix seconds.
type archiveHeader struct {
	flags   byte
	created time.Time
	sum     [sha256.Size]byte
}

// Encode returns the encoded archive bytes. It is the byte-buffered wrapper
// around EncodeFile for callers that need the bytes; the HTTP download path
// uses EncodeFile directly so it never retains a second full archive copy.
// The returned bytes have already round-tripped through the streaming
// self-check.
func Encode(stageDir string, a Archive) ([]byte, error) {
	path, size, err := EncodeFile(stageDir, a, int64(config.BackupMaxBytesMax))
	if err != nil {
		return nil, err
	}
	defer os.Remove(path)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read encoded backup: %w", err)
	}
	if int64(len(raw)) != size {
		return nil, fmt.Errorf("read encoded backup: size changed")
	}
	return raw, nil
}

// EncodeFile streams one self-checked archive into a unique staging file in
// stageDir and returns its path and exact size. stageDir is the volume that
// owns backup-sized transient files (the live database's directory); a
// config-only archive may pass "" and uses the process temp directory, which
// never holds a database-sized member. maxArchiveBytes is the configured
// backup_max_bytes ceiling for the encoded artifact, enforced as the stream
// is written, so a too-large archive is removed instead of served.
//
// The caller owns the returned file and must os.Remove it after serving or
// after giving up on it. On every error path this function removes its own
// staging file and returns an empty path.
func EncodeFile(stageDir string, a Archive, maxArchiveBytes int64) (string, int64, error) {
	if maxArchiveBytes <= 0 {
		return "", 0, fmt.Errorf("backup size limit is invalid")
	}
	if a.flags() == 0 {
		return "", 0, ErrEmptyArchive
	}
	if len(a.Database) > 0 && stageDir == "" {
		return "", 0, fmt.Errorf("snapshot staging directory is required")
	}
	if a.Created.IsZero() {
		a.Created = time.Now().UTC()
	}
	payloadSize := int64(0)
	if len(a.Config) > 0 {
		payloadSize += int64(len(a.Config)) + 9
	}
	if len(a.Database) > 0 {
		payloadSize += int64(len(a.Database)) + 9
	}
	if payloadSize > payloadMax {
		return "", 0, fmt.Errorf("backup payload exceeds backup_max_bytes maximum")
	}

	dir := stageDir
	if dir == "" {
		dir = os.TempDir()
	}
	f, err := os.CreateTemp(stageDir, archiveTempPattern)
	if err != nil {
		return "", 0, fmt.Errorf("stage backup in %s: %w", dir, err)
	}
	stagedPath := f.Name()
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			_ = os.Remove(stagedPath)
		}
	}()

	var placeholder [headerSize]byte
	if err := writeAll(f, placeholder[:]); err != nil {
		return "", 0, fmt.Errorf("stage backup: %w", err)
	}
	cap := &capWriter{w: f, limit: maxArchiveBytes - headerSize}
	if cap.limit < 0 {
		return "", 0, ErrTooLarge
	}
	hasher := sha256.New()
	enc, err := zstd.NewWriter(cap,
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithEncoderCRC(true),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		return "", 0, fmt.Errorf("compress backup: %w", err)
	}
	payload := &payloadWriter{w: enc, h: hasher, limit: payloadMax}
	if err := writeMembers(payload, a); err != nil {
		_ = enc.Close()
		if cap.hit {
			return "", 0, ErrTooLarge
		}
		return "", 0, fmt.Errorf("compress backup: %w", err)
	}
	if err := enc.Close(); err != nil {
		if cap.hit {
			return "", 0, ErrTooLarge
		}
		return "", 0, fmt.Errorf("compress backup: %w", err)
	}
	if cap.hit {
		return "", 0, ErrTooLarge
	}
	var sum [sha256.Size]byte
	copy(sum[:], hasher.Sum(nil))
	if _, err := f.WriteAt(buildHeader(a, sum), 0); err != nil {
		return "", 0, fmt.Errorf("stage backup: %w", err)
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return "", 0, fmt.Errorf("stage backup: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", 0, fmt.Errorf("stage backup: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", 0, fmt.Errorf("stage backup: %w", err)
	}
	if err := selfCheckFile(stagedPath, stageDir, a); err != nil {
		return "", 0, fmt.Errorf("backup self-check: %w", err)
	}
	committed = true
	return stagedPath, size, nil
}

// forEachMember walks the version-1 member framing in its one canonical
// order: config first, then database. Both the streaming encoder and the
// self-check comparison consume this order, so they cannot drift.
func forEachMember(a Archive, fn func(kind uint8, data []byte) error) error {
	if len(a.Config) > 0 {
		if err := fn(memberConfig, a.Config); err != nil {
			return err
		}
	}
	if len(a.Database) > 0 {
		if err := fn(memberDatabase, a.Database); err != nil {
			return err
		}
	}
	return nil
}

func writeMembers(w io.Writer, a Archive) error {
	return forEachMember(a, func(kind uint8, data []byte) error {
		return writeMember(w, kind, data)
	})
}

func writeMember(w io.Writer, kind uint8, data []byte) error {
	var hdr [9]byte
	hdr[0] = kind
	binary.BigEndian.PutUint64(hdr[1:], uint64(len(data)))
	if err := writeAll(w, hdr[:]); err != nil {
		return err
	}
	return writeAll(w, data)
}

func writeAll(w io.Writer, b []byte) error {
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}

// buildHeader is the single version-1 header composer. The caller has already
// finalized Created (a zero time is never encoded).
func buildHeader(a Archive, sum [sha256.Size]byte) []byte {
	out := make([]byte, 0, headerSize)
	out = append(out, magic...)
	out = append(out, archiveVersion, a.flags(), 0, 0)
	out = binary.BigEndian.AppendUint64(out, uint64(a.Created.Unix()))
	out = append(out, sum[:]...)
	return out
}

// capWriter enforces the encoded artifact ceiling while bytes are written.
// It refuses the whole write that would cross the limit, so the staging file
// never grows past maxArchiveBytes and no partial archive can be served.
type capWriter struct {
	w     io.Writer
	limit int64
	n     int64
	hit   bool
}

func (c *capWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > c.limit-c.n {
		c.hit = true
		return 0, ErrTooLarge
	}
	n, err := c.w.Write(b)
	c.n += int64(n)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return n, err
}

// payloadWriter hashes and counts the uncompressed payload (member framing
// included) as it streams into the compressor, so the header SHA-256 and the
// payload cap never need a second materialized copy.
type payloadWriter struct {
	w     io.Writer
	h     hash.Hash
	n     int64
	limit int64
}

func (p *payloadWriter) Write(b []byte) (int, error) {
	if p.n+int64(len(b)) > p.limit {
		return 0, fmt.Errorf("backup payload exceeds backup_max_bytes maximum")
	}
	n, err := p.w.Write(b)
	if n > 0 {
		_, _ = p.h.Write(b[:n])
		p.n += int64(n)
	}
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return n, err
}

// selfCheckFile streams the encoded file back through the decoder, verifies
// the header, member framing, byte equality with the source members, payload
// size and outer SHA-256, then runs the same Validate semantics on those
// verified members. It never reads the whole archive or payload into memory.
func selfCheckFile(path, stageDir string, a Archive) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	hdr := make([]byte, headerSize)
	if _, err := io.ReadFull(f, hdr); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("backup is truncated")
		}
		return err
	}
	h, err := parseHeader(hdr)
	if err != nil {
		return err
	}
	if h.flags != a.flags() || h.created.Unix() != a.Created.Unix() {
		return fmt.Errorf("payload changed")
	}
	dec, err := zstd.NewReader(zstdStream{f},
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(uint64(payloadMax)+decoderWindow),
	)
	if err != nil {
		return fmt.Errorf("decompress backup: %w", err)
	}
	defer dec.Close()
	stream := &hashReader{r: io.LimitReader(dec, payloadMax+1), h: sha256.New()}
	if err := compareMembers(stream, a); err != nil {
		return err
	}
	var extra [1]byte
	if n, err := stream.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("decompress backup: %w", err)
		}
		return fmt.Errorf("payload changed")
	}
	if stream.n > payloadMax {
		return fmt.Errorf("backup payload exceeds maximum size")
	}
	if !bytes.Equal(stream.h.Sum(nil), h.sum[:]) {
		return fmt.Errorf("backup payload hash mismatch")
	}
	return Validate(stageDir, a)
}

// compareMembers consumes the decoder stream in the encoder's canonical
// member order and compares every framed byte with the source member. A
// truncated, reordered, duplicated or altered payload fails here before
// Validate runs.
func compareMembers(r io.Reader, a Archive) error {
	buf := make([]byte, selfCheckBuffer)
	return forEachMember(a, func(kind uint8, want []byte) error {
		var hdr [9]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return truncated(err)
		}
		if hdr[0] != kind || binary.BigEndian.Uint64(hdr[1:]) != uint64(len(want)) {
			return fmt.Errorf("payload changed")
		}
		for off := 0; off < len(want); {
			n := len(want) - off
			if n > len(buf) {
				n = len(buf)
			}
			if _, err := io.ReadFull(r, buf[:n]); err != nil {
				return truncated(err)
			}
			if !bytes.Equal(buf[:n], want[off:off+n]) {
				return fmt.Errorf("payload changed")
			}
			off += n
		}
		return nil
	})
}

func truncated(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("backup member is truncated")
	}
	return err
}

// hashReader hashes and counts the decompressed payload as the self-check
// consumes it.
type hashReader struct {
	r io.Reader
	h hash.Hash
	n int64
}

func (r *hashReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		_, _ = r.h.Write(p[:n])
		r.n += int64(n)
	}
	return n, err
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
	h, err := parseHeader(raw)
	if err != nil {
		return Archive{}, err
	}
	maxMem := uint64(maxPayload) + decoderWindow
	dec, err := zstd.NewReader(zstdStream{bytes.NewReader(raw[headerSize:])},
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(maxMem),
	)
	if err != nil {
		return Archive{}, fmt.Errorf("decompress backup: %w", err)
	}
	defer dec.Close()
	payload, err := readCapped(dec, maxPayload)
	if err != nil {
		return Archive{}, fmt.Errorf("decompress backup: %w", err)
	}
	if sha256.Sum256(payload) != h.sum {
		return Archive{}, fmt.Errorf("backup payload hash mismatch")
	}
	a, err := unmarshalMembers(payload, h.flags)
	if err != nil {
		return Archive{}, err
	}
	a.Created = h.created
	return a, nil
}

// parseHeader validates the fixed version-1 header and returns its fields.
// The body starts at headerSize.
func parseHeader(raw []byte) (archiveHeader, error) {
	if len(raw) < headerSize {
		return archiveHeader{}, fmt.Errorf("backup is truncated")
	}
	if string(raw[:4]) != magic {
		return archiveHeader{}, fmt.Errorf("not a millivolt backup")
	}
	if raw[4] != archiveVersion {
		return archiveHeader{}, fmt.Errorf("unsupported backup version %d", raw[4])
	}
	flags := raw[5]
	if flags == 0 || flags&^knownFlags != 0 {
		return archiveHeader{}, fmt.Errorf("backup flags %02x are not a known config/database set", flags)
	}
	if raw[6] != 0 || raw[7] != 0 {
		return archiveHeader{}, fmt.Errorf("backup header is reserved and must be zero")
	}
	var h archiveHeader
	h.flags = flags
	h.created = time.Unix(int64(binary.BigEndian.Uint64(raw[8:16])), 0).UTC()
	copy(h.sum[:], raw[16:headerSize])
	return h, nil
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
// and a database member must be a packed SQLite snapshot with a requests
// table. stageDir is the directory used to stage a database member for
// inspection (the live database's volume); it is required when the archive
// carries a database member and ignored otherwise.
func Validate(stageDir string, a Archive) error {
	if a.flags() == 0 {
		return ErrEmptyArchive
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
		if err := CheckDatabase(stageDir, a.Database); err != nil {
			return fmt.Errorf("backup database: %w", err)
		}
	}
	return nil
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
