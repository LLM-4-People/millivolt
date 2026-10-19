package backup

import "strings"

// Transient archive/restore staging names created on the live database's
// volume. They are the only names a startup sweep may reclaim; the live
// database, pending admission slot, rollback set and state marker are never
// part of this registry.
const (
	// ArchiveStagingPattern names the one transient encoded artifact created
	// by EncodeFile. The wildcard keeps concurrent encodes distinct. The
	// leading dot makes the name private: the public download artifact is
	// millivolt-backup-<UTC stamp>.mvb, and a retained download must never be
	// mistaken for an orphaned staging file.
	ArchiveStagingPattern = ".millivolt-backup-*.mvb"

	// RestoreDatabaseStagingPattern names the private directory holding a
	// decoded database member.
	RestoreDatabaseStagingPattern = "millivolt-restore-db-*"

	// InspectDatabaseStagingPattern names the private directory holding the
	// copied database member admitted by InspectDatabase.
	InspectDatabaseStagingPattern = "millivolt-backup-db-*"

	// RestoreUploadStagingPattern names one private uploaded archive. The
	// restore handler removes it on every path; a crash can orphan it.
	RestoreUploadStagingPattern = "millivolt-restore-upload-*.mvb"

	// RestorePendingStagingPattern names the private sibling that preserves a
	// pre-existing pending snapshot while a replace restore stages a new one.
	RestorePendingStagingPattern = ".millivolt-restore-pending-*"
)

// stagingPatterns is the one registry of backup/restore transient names.
var stagingPatterns = []string{
	ArchiveStagingPattern,
	RestoreDatabaseStagingPattern,
	InspectDatabaseStagingPattern,
	RestoreUploadStagingPattern,
	RestorePendingStagingPattern,
}

// IsStagingName reports whether base names one of the backup/restore transient
// staging entries. Callers still own the directory scope and the age bound; a
// match here says only that the package created a path with this shape, never
// that it is safe to remove right now.
func IsStagingName(base string) bool {
	for _, pattern := range stagingPatterns {
		if MatchesStagingPattern(pattern, base) {
			return true
		}
	}
	return false
}

// MatchesStagingPattern reports whether base matches one os.CreateTemp or
// os.MkdirTemp pattern. The pattern must contain exactly one '*', and the
// wildcard must expand to at least one byte so an empty random suffix never
// matches a bare prefix.
func MatchesStagingPattern(pattern, base string) bool {
	prefix, suffix, ok := strings.Cut(pattern, "*")
	if !ok {
		return base == pattern
	}
	if strings.Contains(suffix, "*") {
		return false
	}
	return len(base) > len(prefix)+len(suffix) &&
		strings.HasPrefix(base, prefix) &&
		strings.HasSuffix(base, suffix)
}
