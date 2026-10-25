// Package backup is the single owner of millivolt operator backup archives.
//
// A backup is a packed SQLite snapshot (VACUUM INTO) and/or the schema YAML,
// wrapped in a zstd frame with a content checksum and an outer SHA-256 of the
// uncompressed payload. The database member may be held as bytes or named by
// an already-staged private file. EncodeFile is the streaming encode owner: it
// writes one archive into a unique staging file, verifies it, and only then
// returns it. A file-backed database member streams in bounded chunks, so
// encoding never materializes the whole database on the heap. Encode is the
// byte-buffered wrapper for callers that need the bytes. Neither returns an
// archive that Decode+Validate would reject, so a produced archive is never a
// silent corrupt file.
//
// Decode and DecodeFile share one framing, zstd, SHA-256 and member-order
// owner. Decode materializes the members for byte callers; DecodeFile writes a
// database member into a private staging directory instead, so a restore never
// holds the database on the heap. The file-backed decoder removes its staging
// directory on every error and the caller owns it after success.
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
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// ErrEmptyArchive rejects an archive with no members: Encode and Validate
// both require at least one of config or database.
var ErrEmptyArchive = errors.New("backup must include config, database, or both")

// ErrSnapshotStaging reports that a database member needs the live database
// volume for its private staging file. Callers that know storage is disabled
// map it to their own sentinel; the wording stays in one place.
var ErrSnapshotStaging = errors.New("snapshot staging directory is required")

// ErrTooLarge reports an encoded archive or restore upload that would exceed
// the caller's backup_max_bytes ceiling. EncodeFile and the restore upload
// stager enforce the ceiling while streaming, so a partial artifact is never
// returned or served.
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

	// restoreDatabaseName is the decoded member's file inside the
	// RestoreDatabaseStagingPattern directory.
	// The directory is unique, so the name needs no further wildcard.
	restoreDatabaseName = "snapshot.db"

	// selfCheckBuffer bounds the comparison buffer of the streaming
	// self-check; it is reused for every member chunk.
	selfCheckBuffer = 32 << 10

	// configMemberMax bounds the retained config member. It is exactly the
	// config document bound Config.Validate enforces on the canonical rendered
	// YAML, so a config the proxy accepts can always be encoded and decoded;
	// this keeps a crafted archive from forcing a multi-gigabyte heap
	// allocation before config.LoadBytes. It is a retention guardrail, not a
	// database member cap: the database member stays streamed under payloadMax.
	configMemberMax = config.ConfigDocumentMaxBytes
)

// checkConfigMemberSize is the one config-member retention gate. EncodeFile
// rejects a source config above the bound before staging anything, and the
// shared decode framing owner rejects a declared member above it before the
// consumer can copy the body into heap.
func checkConfigMemberSize(size int64) error {
	if size > configMemberMax {
		return fmt.Errorf("backup config member exceeds %d bytes", configMemberMax)
	}
	return nil
}

// zstdStream is an io.Reader that does not implement Bytes or Len, so
// klauspost cannot DecodeAll a compressed body under 128KiB into an
// unbounded buffer before readCapped runs.
type zstdStream struct{ r io.Reader }

func (s zstdStream) Read(p []byte) (int, error) { return s.r.Read(p) }

// newPayloadDecoder is the one zstd reader configuration for both decode
// paths. The decoder memory ceiling is the payload cap plus klauspost's
// default window, so a small frame cannot request unbounded decoder state.
func newPayloadDecoder(src io.Reader, maxPayload int64) (*zstd.Decoder, error) {
	return zstd.NewReader(zstdStream{src},
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(uint64(maxPayload)+decoderWindow),
	)
}

// payloadMax is the uncompressed decode ceiling: the Schema maximum for
// backup_max_bytes. Tests lower it to lock LimitReader without allocating
// that many bytes.
var payloadMax = int64(config.BackupMaxBytesMax)

// Archive is one operator backup. At least one member must be present.
type Archive struct {
	Created time.Time
	Config  []byte
	// Database is the in-memory packed SQLite member. It is mutually
	// exclusive with DatabasePath.
	Database []byte
	// DatabasePath names an already-staged packed SQLite member, normally
	// Store.SnapshotFile. It is mutually exclusive with Database. Encoding
	// streams the file and Validate inspects it in place, so neither copies
	// the database onto the heap. The caller owns the file and its cleanup.
	DatabasePath string
	// DatabasePresent records that the archive declares a database member,
	// including when a selection-aware decode stream-discarded it. Database
	// bytes and path are empty in that case; DatabaseBytes is its decoded size.
	DatabasePresent bool
	DatabaseBytes   int64
}

func (a Archive) flags() byte {
	var f byte
	if len(a.Config) > 0 {
		f |= flagConfig
	}
	if a.hasDatabase() {
		f |= flagDatabase
	}
	return f
}

func (a Archive) hasDatabase() bool {
	return len(a.Database) > 0 || a.DatabasePath != ""
}

// checkMemberSources rejects the one ambiguous archive shape: a database
// member named both as bytes and as a file. Every source-order walk and
// Validate call this one owner before touching a member.
func (a Archive) checkMemberSources() error {
	if len(a.Database) > 0 && a.DatabasePath != "" {
		return fmt.Errorf("backup database member has both bytes and a path")
	}
	return nil
}

// DatabaseMemberSize returns the database member's exact byte length without
// materializing it: the slice length for bytes or the file's stat for a path.
// It is zero when the archive has no database member. Callers that report a
// member size use this one walk so it cannot disagree with the encoder.
func (a Archive) DatabaseMemberSize() (int64, error) {
	var size int64
	err := forEachMember(a, func(m memberSource) error {
		if m.kind == memberDatabase {
			size = m.size
		}
		return nil
	})
	return size, err
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
// A path-backed database member is encoded from its file before this wrapper
// reads the final archive, so the source database is never copied into heap.
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
	if err := a.checkMemberSources(); err != nil {
		return "", 0, err
	}
	if err := checkConfigMemberSize(int64(len(a.Config))); err != nil {
		return "", 0, err
	}
	if a.flags() == 0 {
		return "", 0, ErrEmptyArchive
	}
	if a.hasDatabase() && stageDir == "" {
		return "", 0, ErrSnapshotStaging
	}
	if a.Created.IsZero() {
		a.Created = time.Now().UTC()
	}
	// The member walk owns source order, framing overhead and a file-backed
	// member's length (from its stat), so the early payload cap and the encoded
	// stream can never disagree about which members exist. The addition is
	// checked: an implausibly large sparse source saturates the count instead
	// of wrapping negative and slipping past the cap.
	payloadSize := int64(0)
	if err := forEachMember(a, func(m memberSource) error {
		if m.size > payloadMax-9-payloadSize {
			payloadSize = payloadMax + 1
			return nil
		}
		payloadSize += m.size + 9
		return nil
	}); err != nil {
		return "", 0, err
	}
	if payloadSize > payloadMax {
		return "", 0, fmt.Errorf("backup payload exceeds backup_max_bytes maximum")
	}

	dir := stageDir
	if dir == "" {
		dir = os.TempDir()
	}
	f, err := os.CreateTemp(stageDir, ArchiveStagingPattern)
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

// memberSource is one framed member in the version-1 source order. Exactly
// one of data (in-memory) and path (file-backed) is set; size is the member's
// byte length, from the slice for bytes or from the file's stat for a path.
type memberSource struct {
	kind uint8
	size int64
	data []byte
	path string
}

// databaseStreamHook is a test seam (production leaves it nil). It runs after
// a file-backed member's header is written and the file is opened, so a test
// can change the file's size and prove the stream fails closed.
var databaseStreamHook func(path string)

// forEachMember walks the version-1 member framing in its one canonical
// order: config first, then database. The encoder, the payload-size precheck
// and the self-check comparison all consume this order, so they cannot drift.
// A file-backed database member is sized once per walk; its bytes stay on
// disk until a consumer opens it.
func forEachMember(a Archive, fn func(m memberSource) error) error {
	if err := a.checkMemberSources(); err != nil {
		return err
	}
	if len(a.Config) > 0 {
		if err := fn(memberSource{kind: memberConfig, size: int64(len(a.Config)), data: a.Config}); err != nil {
			return err
		}
	}
	switch {
	case len(a.Database) > 0:
		return fn(memberSource{kind: memberDatabase, size: int64(len(a.Database)), data: a.Database})
	case a.DatabasePath != "":
		info, err := os.Stat(a.DatabasePath)
		if err != nil {
			return fmt.Errorf("inspect database member %s: %w", a.DatabasePath, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("database member %s is not a regular file", a.DatabasePath)
		}
		if info.Size() <= 0 {
			return fmt.Errorf("database member %s is empty", a.DatabasePath)
		}
		return fn(memberSource{kind: memberDatabase, size: info.Size(), path: a.DatabasePath})
	}
	return nil
}

func writeMembers(w io.Writer, a Archive) error {
	return forEachMember(a, func(m memberSource) error {
		return writeMember(w, m)
	})
}

func writeMember(w io.Writer, m memberSource) error {
	var hdr [9]byte
	hdr[0] = m.kind
	binary.BigEndian.PutUint64(hdr[1:], uint64(m.size))
	if err := writeAll(w, hdr[:]); err != nil {
		return err
	}
	if m.path == "" {
		return writeAll(w, m.data)
	}
	return streamDatabaseMember(w, m)
}

// streamDatabaseMember copies exactly the length captured for a file-backed
// database member, in bounded chunks. A file that shrank, grew, or cannot be
// read fails the write: the framed length and the bytes actually written must
// agree, so a changed snapshot is never silently truncated.
func streamDatabaseMember(w io.Writer, m memberSource) error {
	f, err := os.Open(m.path)
	if err != nil {
		return fmt.Errorf("open database member %s: %w", m.path, err)
	}
	defer f.Close()
	if databaseStreamHook != nil {
		databaseStreamHook(m.path)
	}
	buf := make([]byte, selfCheckBuffer)
	for off := int64(0); off < m.size; {
		n := m.size - off
		if n > int64(len(buf)) {
			n = int64(len(buf))
		}
		if _, err := io.ReadFull(f, buf[:n]); err != nil {
			return fmt.Errorf("database member %s changed size while encoding: %w", m.path, err)
		}
		if err := writeAll(w, buf[:n]); err != nil {
			return err
		}
		off += n
	}
	var extra [1]byte
	if n, err := f.Read(extra[:]); n != 0 {
		return fmt.Errorf("database member %s changed size while encoding: file grew", m.path)
	} else if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read database member %s: %w", m.path, err)
	}
	return nil
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
	dec, err := newPayloadDecoder(f, payloadMax)
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
	if err := checkPayloadHash(stream.h.Sum(nil), h.sum); err != nil {
		return err
	}
	return Validate(stageDir, a)
}

// compareMembers consumes the decoder stream in the encoder's canonical
// member order and compares every framed byte with the source member. A
// truncated, reordered, duplicated or altered payload fails here before
// Validate runs. A file-backed member is compared in bounded chunks and is
// never loaded whole.
func compareMembers(r io.Reader, a Archive) error {
	decoded := make([]byte, selfCheckBuffer)
	source := make([]byte, selfCheckBuffer)
	return forEachMember(a, func(m memberSource) error {
		var hdr [9]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return truncated(err)
		}
		if hdr[0] != m.kind || binary.BigEndian.Uint64(hdr[1:]) != uint64(m.size) {
			return fmt.Errorf("payload changed")
		}
		var src io.Reader
		if m.path == "" {
			src = bytes.NewReader(m.data)
		} else {
			f, err := os.Open(m.path)
			if err != nil {
				return fmt.Errorf("open database member %s: %w", m.path, err)
			}
			defer f.Close()
			src = f
		}
		for off := int64(0); off < m.size; {
			n := m.size - off
			if n > int64(len(decoded)) {
				n = int64(len(decoded))
			}
			if _, err := io.ReadFull(src, source[:n]); err != nil {
				return fmt.Errorf("source member changed size: %w", err)
			}
			if _, err := io.ReadFull(r, decoded[:n]); err != nil {
				return truncated(err)
			}
			if !bytes.Equal(decoded[:n], source[:n]) {
				return fmt.Errorf("payload changed")
			}
			off += n
		}
		var extra [1]byte
		if n, err := src.Read(extra[:]); n != 0 {
			return fmt.Errorf("source member changed size: file grew")
		} else if err != nil && !errors.Is(err, io.EOF) {
			return err
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

// checkPayloadHash is the one outer SHA-256 comparison for both decoders, so
// the byte and file paths cannot disagree about hash admission or the error
// wording.
func checkPayloadHash(got []byte, want [sha256.Size]byte) error {
	if !bytes.Equal(got, want[:]) {
		return fmt.Errorf("backup payload hash mismatch")
	}
	return nil
}

// Decode admits an archive only when magic, version, zstd checksum, and the
// outer SHA-256 all match. Uncompressed members cannot exceed the Schema
// maximum for backup_max_bytes, so a small frame cannot expand without bound.
// It does not apply config or open SQLite. It is the byte-buffered wrapper over
// the shared decoder; DecodeFile is the file-backed owner for restore.
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
	dec, err := newPayloadDecoder(bytes.NewReader(raw[headerSize:]), maxPayload)
	if err != nil {
		return Archive{}, fmt.Errorf("decompress backup: %w", err)
	}
	defer dec.Close()
	payload, err := readCapped(dec, maxPayload)
	if err != nil {
		return Archive{}, fmt.Errorf("decompress backup: %w", err)
	}
	sum := sha256.Sum256(payload)
	if err := checkPayloadHash(sum[:], h.sum); err != nil {
		return Archive{}, err
	}
	var a Archive
	err = decodeMembers(bytes.NewReader(payload), h.flags, maxPayload, func(kind uint8, size int64, r io.Reader) error {
		data, err := readMemberBytes(r, size)
		if err != nil {
			return err
		}
		switch kind {
		case memberConfig:
			a.Config = data
		case memberDatabase:
			a.Database = data
		}
		return nil
	})
	if err != nil {
		return Archive{}, err
	}
	a.Created = h.created
	return a, nil
}

// DecodeFile is the file-backed decoder for restore. It validates the archive
// at path with the same header, zstd, SHA-256, framing and member rules as
// Decode, but writes a database member into a unique private directory under
// stageDir instead of holding it on the heap. stageDir must name the live
// database's volume when the archive declares a database member and may be
// empty for a config-only archive. On success the returned DatabasePath names
// the decoded packed database and the caller owns its directory
// (filepath.Dir(a.DatabasePath)); only config bytes stay buffered. On every
// error the decoder removes the decoded file and its staging directory and
// returns no Archive.
func DecodeFile(stageDir, path string) (Archive, error) {
	return DecodeFileSelected(stageDir, path, true)
}

// DecodeFileSelected is DecodeFile with an explicit database selection. When
// keepDatabase is false, a declared database member is stream-discarded while
// the outer payload hash and framing are still checked, so a config-only
// restore or preview needs no live database volume and no SQLite staging.
func DecodeFileSelected(stageDir, path string, keepDatabase bool) (Archive, error) {
	if path == "" {
		return Archive{}, fmt.Errorf("backup path is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return Archive{}, fmt.Errorf("open backup: %w", err)
	}
	defer f.Close()
	return decodeFileSelected(stageDir, f, payloadMax, keepDatabase)
}

func decodeFile(stageDir string, src io.Reader, maxPayload int64) (Archive, error) {
	return decodeFileSelected(stageDir, src, maxPayload, true)
}

// decodeFileSelected is the one file-backed decode owner. Its member sink
// writes the database body straight to a private file while the shared framing
// owner keeps the payload hash, cap and admission checks in one place.
func decodeFileSelected(stageDir string, src io.Reader, maxPayload int64, keepDatabase bool) (Archive, error) {
	if maxPayload <= 0 {
		return Archive{}, fmt.Errorf("backup payload limit is invalid")
	}
	hdr := make([]byte, headerSize)
	if _, err := io.ReadFull(src, hdr); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return Archive{}, fmt.Errorf("backup is truncated")
		}
		return Archive{}, fmt.Errorf("read backup header: %w", err)
	}
	h, err := parseHeader(hdr)
	if err != nil {
		return Archive{}, err
	}
	if h.flags&flagDatabase != 0 && stageDir == "" && keepDatabase {
		return Archive{}, ErrSnapshotStaging
	}
	dec, err := newPayloadDecoder(src, maxPayload)
	if err != nil {
		return Archive{}, fmt.Errorf("decompress backup: %w", err)
	}
	defer dec.Close()
	// The hash reader sees every decompressed payload byte, including member
	// framing, and the limit reader stops an expansion bomb one byte past the
	// cap so the over-cap owner can answer before any staging file is kept.
	stream := &hashReader{
		r: &decompressReader{r: io.LimitReader(dec, maxPayload+1)},
		h: sha256.New(),
	}
	var a Archive
	var dbDir, dbPath string
	var db *os.File
	cleanup := func() {
		if db != nil {
			_ = db.Close()
			db = nil
		}
		if dbDir != "" {
			_ = os.RemoveAll(dbDir)
			dbDir = ""
		}
	}
	err = decodeMembers(stream, h.flags, maxPayload, func(kind uint8, size int64, r io.Reader) error {
		switch kind {
		case memberConfig:
			data, err := readMemberBytes(r, size)
			if err != nil {
				return err
			}
			a.Config = data
			return nil
		case memberDatabase:
			a.DatabasePresent = true
			a.DatabaseBytes = size
			if !keepDatabase {
				n, err := io.CopyBuffer(io.Discard, r, make([]byte, selfCheckBuffer))
				if err != nil {
					return err
				}
				if n != size {
					return fmt.Errorf("backup member is truncated")
				}
				return nil
			}
			if db == nil {
				dir, err := os.MkdirTemp(stageDir, RestoreDatabaseStagingPattern)
				if err != nil {
					return StageSnapshotError(stageDir, err)
				}
				dbDir = dir
				dbPath = filepath.Join(dir, restoreDatabaseName)
				f, err := os.OpenFile(dbPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					return err
				}
				db = f
			}
			n, err := io.CopyBuffer(db, r, make([]byte, selfCheckBuffer))
			if err != nil {
				return err
			}
			if n != size {
				return fmt.Errorf("backup member is truncated")
			}
			return nil
		}
		return nil
	})
	if stream.n > maxPayload {
		cleanup()
		return Archive{}, fmt.Errorf("decompress backup: backup payload exceeds maximum size")
	}
	if err != nil {
		cleanup()
		return Archive{}, err
	}
	if err := checkPayloadHash(stream.h.Sum(nil), h.sum); err != nil {
		cleanup()
		return Archive{}, err
	}
	if db != nil {
		if err := db.Close(); err != nil {
			cleanup()
			return Archive{}, fmt.Errorf("close decoded database: %w", err)
		}
		db = nil
		a.DatabasePath = dbPath
	}
	a.Created = h.created
	return a, nil
}

// memberConsumer consumes one member's exact body. The shared framing owner
// has already accepted the member's kind, flag and uniqueness when it runs.
type memberConsumer func(kind uint8, size int64, r io.Reader) error

// decodeMembers is the single version-1 member framing owner. Both decoders
// consume their payload through it, so member order, duplicate/unknown/missing
// rejection, truncation and the flag rules cannot drift between the byte and
// file paths. A consumer must read exactly size bytes; a short body is
// rejected here even when the consumer reports no error.
func decodeMembers(r io.Reader, flags byte, limit int64, consume memberConsumer) error {
	seen := map[uint8]bool{}
	sizes := map[uint8]int64{}
	for {
		var hdr [9]byte
		n, err := io.ReadFull(r, hdr[:])
		if errors.Is(err, io.EOF) && n == 0 {
			break
		}
		if err != nil {
			return truncated(err)
		}
		kind := hdr[0]
		size := binary.BigEndian.Uint64(hdr[1:])
		if size > uint64(limit) {
			return fmt.Errorf("backup payload exceeds maximum size")
		}
		if seen[kind] {
			return fmt.Errorf("backup has a duplicate member")
		}
		seen[kind] = true
		switch kind {
		case memberConfig:
			if flags&flagConfig == 0 {
				return fmt.Errorf("backup config member is not flagged")
			}
			if err := checkConfigMemberSize(int64(size)); err != nil {
				return err
			}
		case memberDatabase:
			if flags&flagDatabase == 0 {
				return fmt.Errorf("backup database member is not flagged")
			}
		default:
			return fmt.Errorf("unknown backup member %d", kind)
		}
		lr := &io.LimitedReader{R: r, N: int64(size)}
		if err := consume(kind, int64(size), lr); err != nil {
			return err
		}
		if lr.N != 0 {
			return fmt.Errorf("backup member is truncated")
		}
		sizes[kind] = int64(size)
	}
	if flags&flagConfig != 0 && sizes[memberConfig] == 0 {
		return fmt.Errorf("backup is missing its config member")
	}
	if flags&flagDatabase != 0 && sizes[memberDatabase] == 0 {
		return fmt.Errorf("backup is missing its database member")
	}
	return nil
}

// readMemberBytes reads one exact-size member into heap. A zero-size member
// stays nil, matching the byte decoder's empty-member meaning.
func readMemberBytes(r io.Reader, size int64) ([]byte, error) {
	if size == 0 {
		return nil, nil
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, truncated(err)
	}
	return data, nil
}

// decompressReader names the zstd frame as the cause of a streaming read
// failure, matching the byte decoder's "decompress backup" wrapping.
type decompressReader struct{ r io.Reader }

func (d decompressReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("decompress backup: %w", err)
	}
	return n, err
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
// table. stageDir is the directory used to stage an in-memory database member
// for inspection (the live database's volume) and is ignored for a
// file-backed member, which is integrity-checked in place without a second
// copy.
func Validate(stageDir string, a Archive) error {
	return ValidateSelected(stageDir, a, true)
}

// ValidateSelected is Validate with an explicit database selection. An
// unselected database member was stream-discarded after its framing and outer
// hash were checked, so only config semantics remain to validate.
func ValidateSelected(stageDir string, a Archive, validateDatabase bool) error {
	if err := a.checkMemberSources(); err != nil {
		return err
	}
	if a.flags() == 0 && !a.DatabasePresent {
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
	if validateDatabase {
		switch {
		case len(a.Database) > 0:
			if err := CheckDatabase(stageDir, a.Database); err != nil {
				return fmt.Errorf("backup database: %w", err)
			}
		case a.DatabasePath != "":
			if err := CheckDatabaseFile(a.DatabasePath); err != nil {
				return fmt.Errorf("backup database: %w", err)
			}
		}
	}
	return nil
}
