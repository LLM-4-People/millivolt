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
	"regexp"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/settingsdoc"
)

// settingsBlock is one generated setting or flag block, keyed by its opening
// bold code span. seen counts duplicate openings so a key cannot appear twice.
type settingsBlock struct {
	line int
	text string
	seen int
}

var blockKey = regexp.MustCompile("^#### `([^`]+)`")

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
	blocks := parseBlocks(t, text)
	settingCount := 0

	for _, field := range config.Schema() {
		block, ok := blocks[field.Key]
		if !ok {
			t.Errorf("setting %q has no block; run go run ./cmd/docgen -write", field.Key)
			continue
		}
		settingCount++
		if block.seen != 1 {
			t.Errorf("setting %q has %d blocks, want exactly one", field.Key, block.seen)
		}
		flat := strings.Join(strings.Fields(block.text), " ")
		if !strings.Contains(flat, field.Label) {
			t.Errorf("%s:%d: %q block does not carry the schema label %q",
				settingsdoc.PagePath, block.line, field.Key, field.Label)
		}
		// The block may insert paragraph breaks inside a long help string, so
		// compare the whitespace-collapsed text. Words and punctuation are
		// already byte-identical because the renderer only changes whitespace.
		help := strings.Join(strings.Fields(field.Help), " ")
		if !strings.Contains(flat, help) {
			t.Errorf("%s:%d: %q help does not match the schema help",
				settingsdoc.PagePath, block.line, field.Key)
		}
		if !strings.Contains(flat, "Default") {
			t.Errorf("%s:%d: %q block has no default", settingsdoc.PagePath, block.line, field.Key)
		}
		if !strings.Contains(flat, "Accepted") {
			t.Errorf("%s:%d: %q block has no accepted-values line", settingsdoc.PagePath, block.line, field.Key)
		}
		wantReload := "Restart required"
		if field.HotReload {
			wantReload = "Hot-reloads"
		}
		if !strings.Contains(flat, wantReload) {
			t.Errorf("%s:%d: %q block does not say %q (schema hot_reload=%v)",
				settingsdoc.PagePath, block.line, field.Key, wantReload, field.HotReload)
		}
		for _, choice := range field.Choices {
			if !strings.Contains(flat, "`"+choice+"`") {
				t.Errorf("%s:%d: %q block is missing the choice %q",
					settingsdoc.PagePath, block.line, field.Key, choice)
			}
		}
		if want := field.RangeText(); want != "" && !strings.Contains(flat, want) {
			t.Errorf("%s:%d: %q block is missing the bound %q",
				settingsdoc.PagePath, block.line, field.Key, want)
		}
	}
	if settingCount != len(config.Schema()) {
		t.Errorf("the page has %d setting blocks, want one per schema field (%d)",
			settingCount, len(config.Schema()))
	}

	flags, err := settingsdoc.Flags(root)
	if err != nil {
		t.Fatalf("discover proxy flags: %v", err)
	}
	flagCount := 0
	for _, flag := range flags {
		key := "-" + flag.Name
		block, ok := blocks[key]
		if !ok {
			t.Errorf("flag %q has no block; run go run ./cmd/docgen -write", key)
			continue
		}
		flagCount++
		if block.seen != 1 {
			t.Errorf("flag %q has %d blocks, want exactly one", key, block.seen)
		}
		flat := strings.Join(strings.Fields(block.text), " ")
		if !strings.Contains(flat, "`"+flag.Type+"`") {
			t.Errorf("%s:%d: flag %q type is not %q", settingsdoc.PagePath, block.line, key, flag.Type)
		}
		if !strings.Contains(flat, "`"+flag.Default+"`") {
			t.Errorf("%s:%d: flag %q default is not the registration default %q",
				settingsdoc.PagePath, block.line, key, flag.Default)
		}
		usage := strings.Join(strings.Fields(flag.Usage), " ")
		if !strings.Contains(flat, usage) {
			t.Errorf("%s:%d: flag %q usage does not match the registration", settingsdoc.PagePath, block.line, key)
		}
	}
	if got := len(blocks); got != len(config.Schema())+len(flags) {
		t.Errorf("the page has %d blocks, want one per schema field and flag (%d)",
			got, len(config.Schema())+len(flags))
	}
	if flagCount != len(flags) {
		t.Errorf("the page has %d flag blocks, want %d", flagCount, len(flags))
	}
}

// parseBlocks indexes the generated blocks by the bold code span that opens
// each one. A block ends at the next opening or at a section heading.
func parseBlocks(t *testing.T, text string) map[string]*settingsBlock {
	t.Helper()
	blocks := map[string]*settingsBlock{}
	var current *settingsBlock
	for index, line := range strings.Split(text, "\n") {
		if match := blockKey.FindStringSubmatch(line); match != nil {
			key := match[1]
			if existing, ok := blocks[key]; ok {
				existing.seen++
				current = nil
				continue
			}
			current = &settingsBlock{line: index + 1, seen: 1, text: line + "\n"}
			blocks[key] = current
			continue
		}
		if strings.HasPrefix(line, "## ") {
			current = nil
			continue
		}
		if current != nil {
			current.text += line + "\n"
		}
	}
	if len(blocks) == 0 {
		t.Fatalf("%s has no setting blocks", settingsdoc.PagePath)
	}
	return blocks
}

// generatedDefaults reads the default value each block renders: the fenced
// YAML block when the value is long, or the inline code span after "Default".
// It is the one reader for prose-default comparisons, so the page layout can
// change without teaching a second test a second shape.
func generatedDefaults(t *testing.T, text string) map[string]string {
	t.Helper()
	defaults := map[string]string{}
	for key, block := range parseBlocks(t, text) {
		if strings.HasPrefix(key, "-") {
			continue
		}
		body := block.text
		const fenced = "Default:\n\n```yaml\n"
		if at := strings.Index(body, fenced); at >= 0 {
			rest := body[at+len(fenced):]
			if end := strings.Index(rest, "\n```"); end >= 0 {
				defaults[key] = rest[:end]
				continue
			}
		}
		const inline = "Default `"
		if at := strings.Index(body, inline); at >= 0 {
			rest := body[at+len(inline):]
			if end := strings.Index(rest, "`"); end >= 0 {
				defaults[key] = rest[:end]
			}
		}
	}
	if len(defaults) == 0 {
		t.Fatalf("%s has no generated defaults", settingsdoc.PagePath)
	}
	return defaults
}
