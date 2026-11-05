// Package params owns the backup and restore query grammar shared by the HTTP
// handlers and the operator command. It is deliberately dependency-free: a
// reader that only needs a key name, boolean spelling, restore mode or the
// archive magic does not link the encoder, SQLite or zstd.
package params

import (
	"errors"
	"strings"
)

// errInvalidRestoreMode is shared by every caller that validates an explicit
// mode, so the accepted grammar and its message cannot drift.
var (
	errEmptyRestoreMode   = errors.New("empty value; want merge or replace")
	errInvalidRestoreMode = errors.New("want merge or replace")
	errInvalidQueryBool   = errors.New("want 1 or 0")
)

// The backup and restore query grammar. These names are the closed set of
// keys the two operator routes consume; the command-line client and the HTTP
// handlers read the same owner, so a key cannot be added on one side only.
const (
	ParamInspect      = "inspect"
	ParamConfig       = "config"
	ParamDatabase     = "database"
	ParamConfigMode   = "config_mode"
	ParamDatabaseMode = "database_mode"
)

// ModeMerge and ModeReplace are the two restore modes. The empty value is
// never a mode: an omitted query key takes ModeReplace, while an explicitly
// empty value is rejected by ParseRestoreMode.
const (
	ModeMerge   = "merge"
	ModeReplace = "replace"
)

// Protocol defaults for an omitted query key. The HTTP handlers and the
// command's settings surface both read these; the command still sends the
// selected values explicitly so "both off" cannot fall back to "both on".
const (
	DefaultConfig      = true
	DefaultDatabase    = true
	DefaultRestoreMode = ModeReplace
)

// BackupQueryKeys and RestoreQueryKeys return the consumed query-key sets.
// Each call builds a fresh slice from the Param constants: adminjson's gates
// need a variadic list, and an exported mutable slice would let any importer
// widen the accepted grammar for every later caller.
func BackupQueryKeys() []string {
	return []string{ParamConfig, ParamDatabase}
}

func RestoreQueryKeys() []string {
	return []string{ParamInspect, ParamConfig, ParamDatabase, ParamConfigMode, ParamDatabaseMode}
}

// ParseBoolParam parses the on/off grammar the backup and restore query keys
// use. It accepts only the documented spellings; an invalid value must never
// silently select the broader default.
func ParseBoolParam(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	default:
		return false, errInvalidQueryBool
	}
}

// BoolParam renders a boolean in the same grammar, so callers never hand-copy
// the "1"/"0" literals.
func BoolParam(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

// ParseRestoreMode normalizes and validates one explicit mode value. It does
// not accept the empty string: an explicit empty value names no mode and must
// not silently select the broader replace action. The command flag and the
// HTTP query key share this normalization so neither can accept a spelling the
// other rejects.
func ParseRestoreMode(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return "", errEmptyRestoreMode
	}
	switch normalized {
	case ModeMerge:
		return ModeMerge, nil
	case ModeReplace:
		return ModeReplace, nil
	default:
		return "", errInvalidRestoreMode
	}
}

// Magic is the four-byte prefix every millivolt archive starts with. It lives
// here with the rest of the archive grammar so a reader that only needs the
// prefix does not link the encoder, SQLite or zstd.
const Magic = "MVB1"

// HasMagic reports whether prefix starts with the archive magic. It lets a
// streaming download reject a captive portal or an HTML error page before the
// file is published, without decoding the archive.
func HasMagic(prefix []byte) bool {
	return len(prefix) >= len(Magic) && string(prefix[:len(Magic)]) == Magic
}
