package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// RenameProviders merges provider labels in stored history: every row whose
// provider matches an alias key is rewritten to its canonical label. Driven by
// config (provider_aliases) - a label split by a naming-scheme change is
// repaired generically, never with provider-specific code. Serialization with
// the writer goroutine comes from the single-connection write pool (the same
// shape a filtered Clear relies on); totals are label-independent, so no
// rebuild is needed. Malformed pairs are skipped (config Validate already rejects them -
// this is the last-chance deny), and each UPDATE is parameterized.
//
// All aliases commit as one transaction: a trigger or constraint failure on a
// later alias rolls the earlier rewrites back instead of leaving history split
// across old and new labels while the caller reports a failed merge. Pairs are
// applied in source-key order so the outcome (and trigger failure point) does
// not depend on Go map iteration order. An error returns 0 rows: nothing from
// the rolled-back transaction was renamed.
func (s *Store) RenameProviders(ctx context.Context, aliases map[string]string) (int64, error) {
	if s == nil || len(aliases) == 0 {
		return 0, nil
	}
	type pair struct{ from, to string }
	pairs := make([]pair, 0, len(aliases))
	for from, to := range aliases {
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if from == "" || to == "" || from == to {
			continue
		}
		pairs = append(pairs, pair{from: from, to: to})
	}
	if len(pairs) == 0 {
		return 0, nil
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].from < pairs[j].from })
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var total int64
	for _, p := range pairs {
		res, err := tx.ExecContext(ctx, "UPDATE requests SET provider = ? WHERE provider = ?", p.to, p.from)
		if err != nil {
			return 0, fmt.Errorf("rename provider %q: %w", p.from, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit provider aliases: %w", err)
	}
	return total, nil
}
