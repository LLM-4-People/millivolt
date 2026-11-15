package config_test

// The generated settings reference (docs/settings.md) is the one page that
// lists every user-tunable setting and every proxy CLI flag. This guard keeps
// it honest in two directions: the page must be byte-for-byte the current
// render, and every schema field and flag registration must reach its own row
// with the owner's own prose. The second half is what a renderer regression
// cannot hide behind: a page that matched a broken renderer byte for byte
// would still fail here.
//
// A stale page is never fixed by editing it. The failure names the command.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/settingsdoc"
)

// settingsRow is one parsed table row, keyed by the code span in its first
// cell (a setting key or a -flag name).
type settingsRow struct {
	line  int
	cells []string
	seen  int
}

func TestSettingsReferenceMatchesTheCode(t *testing.T) {
	root := filepath.Join("..", "..")
	if err := settingsdoc.Check(root); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestSettingsReferenceCoversEveryOwner(t *testing.T) {
	root := filepath.Join("..", "..")
	page, err := os.ReadFile(filepath.Join(root, settingsdoc.PagePath))
	if err != nil {
		t.Fatalf("read %s: %v; run go run ./cmd/docgen -write", settingsdoc.PagePath, err)
	}
	text := string(page)
	if !strings.Contains(text, "go run ./cmd/docgen -write") {
		t.Error("the page does not name the command that regenerates it")
	}
	rows := parseRows(t, text)

	for _, field := range config.Schema() {
		row, ok := rows[field.Key]
		if !ok {
			t.Errorf("setting %q has no row; run go run ./cmd/docgen -write", field.Key)
			continue
		}
		if row.seen != 1 {
			t.Errorf("setting %q has %d rows, want exactly one", field.Key, row.seen)
		}
		if len(row.cells) != 7 {
			t.Errorf("%s:%d: setting %q has %d cells, want 7", settingsdoc.PagePath, row.line, field.Key, len(row.cells))
			continue
		}
		if got := unescape(row.cells[1]); got != field.Label {
			t.Errorf("%s:%d: %q label = %q, want the schema label %q",
				settingsdoc.PagePath, row.line, field.Key, got, field.Label)
		}
		if got := unescape(row.cells[6]); got != field.Help {
			t.Errorf("%s:%d: %q description does not match the schema help",
				settingsdoc.PagePath, row.line, field.Key)
		}
		wantReload := "Restart required"
		if field.HotReload {
			wantReload = "Hot-reloads"
		}
		if got := unescape(row.cells[5]); got != wantReload {
			t.Errorf("%s:%d: %q reload = %q, want %q (schema hot_reload=%v)",
				settingsdoc.PagePath, row.line, field.Key, got, wantReload, field.HotReload)
		}
		if len(field.Choices) == 0 {
			if want := field.RangeText(); want != "" {
				if got := unescape(row.cells[4]); got != want {
					t.Errorf("%s:%d: %q accepted values = %q, want the schema bound %q",
						settingsdoc.PagePath, row.line, field.Key, got, want)
				}
			}
		}
		if got := row.cells[3]; !strings.HasPrefix(got, "`") || !strings.HasSuffix(got, "`") {
			t.Errorf("%s:%d: %q default %q is not a rendered code value",
				settingsdoc.PagePath, row.line, field.Key, got)
		}
	}
	if got := countSettingRows(rows); got != len(config.Schema()) {
		t.Errorf("the page has %d setting rows, want one per schema field (%d)", got, len(config.Schema()))
	}

	flags, err := settingsdoc.Flags(root)
	if err != nil {
		t.Fatalf("discover proxy flags: %v", err)
	}
	for _, flag := range flags {
		key := "-" + flag.Name
		row, ok := rows[key]
		if !ok {
			t.Errorf("flag %q has no row; run go run ./cmd/docgen -write", key)
			continue
		}
		if row.seen != 1 {
			t.Errorf("flag %q has %d rows, want exactly one", key, row.seen)
		}
		if len(row.cells) != 4 {
			t.Errorf("%s:%d: flag %q has %d cells, want 4", settingsdoc.PagePath, row.line, key, len(row.cells))
			continue
		}
		if got := unescape(row.cells[2]); got != "`"+flag.Default+"`" {
			t.Errorf("%s:%d: flag %q default = %q, want %q from the registration",
				settingsdoc.PagePath, row.line, key, got, flag.Default)
		}
		if got := unescape(row.cells[3]); got != flag.Usage {
			t.Errorf("%s:%d: flag %q usage = %q, want the registered usage %q",
				settingsdoc.PagePath, row.line, key, got, flag.Usage)
		}
	}
}

// parseRows indexes the table rows of the page by the code span in the first
// cell. A cell separator is " | "; a literal pipe inside prose is escaped, so
// it never splits a row.
func parseRows(t *testing.T, text string) map[string]*settingsRow {
	t.Helper()
	rows := map[string]*settingsRow{}
	for index, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "| `") || !strings.HasSuffix(line, " |") {
			continue
		}
		body := strings.TrimSuffix(strings.TrimPrefix(line, "| "), " |")
		cells := strings.Split(body, " | ")
		key := strings.Trim(cells[0], "`")
		row, ok := rows[key]
		if !ok {
			row = &settingsRow{line: index + 1, cells: cells}
			rows[key] = row
		}
		row.seen++
	}
	if len(rows) == 0 {
		t.Fatalf("%s has no table rows", settingsdoc.PagePath)
	}
	return rows
}

// countSettingRows counts rows whose first cell is a setting key rather than a
// -flag name, so a stale row for a removed setting is visible too.
func countSettingRows(rows map[string]*settingsRow) int {
	count := 0
	for key := range rows {
		if !strings.HasPrefix(key, "-") {
			count++
		}
	}
	return count
}

// unescape recovers the owner's text from a rendered cell: the only escaping
// the renderer applies is the table's column separator.
func unescape(cell string) string {
	return strings.ReplaceAll(cell, `\|`, "|")
}
