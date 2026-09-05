package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// WriteFile atomically replaces path with a complete, schema-commented YAML
// snapshot of c. Every Schema() key is written; LoadFile's mergeOverlay copies
// a field when its key is present, so an explicit 0 / "" / false survives.
// Default() remains the only place defaults are defined.
func WriteFile(path string, c *Config) error {
	if path == "" {
		return fmt.Errorf("config path is empty")
	}
	var buf bytes.Buffer
	if err := WriteYAML(&buf, c); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	// Preserve an existing config's access restrictions. A new file keeps
	// CreateTemp's private mode; provider header values may contain secrets.
	if info, err := os.Stat(path); err == nil {
		if err := tmp.Chmod(info.Mode().Perm()); err != nil {
			return fmt.Errorf("write config: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("write config: %w", err)
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// WriteYAML renders c as documented YAML using Schema() as the single
// source of comments (type, range, default, hot-reload).
func WriteYAML(w io.Writer, c *Config) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	def := Default()
	values := c.Map()
	defaults := def.Map()

	var b strings.Builder
	b.WriteString("# millivolt configuration.\n")
	b.WriteString("#\n")
	b.WriteString("# Every server setting lives here - there are NO hardcoded configurables in the\n")
	b.WriteString("# code. All keys are optional; omitting one uses the built-in default (shown for\n")
	b.WriteString("# each key below). Every value is validated for type and range on load (a bad\n")
	b.WriteString("# value fails startup with a clear error, never silently applied).\n")
	b.WriteString("#\n")
	b.WriteString("# This file is generated from internal/config.Schema (the single registry the\n")
	b.WriteString("# dashboard Settings menu also uses). Copy proxy.example.yaml to a private\n")
	b.WriteString("# runtime config such as proxy.yaml. Edit that private file or use Settings;\n")
	b.WriteString("# never use the committed example as writable runtime configuration.\n")
	b.WriteString("#\n")
	b.WriteString("# Types:\n")
	b.WriteString("#   string    plain text\n")
	b.WriteString("#   int       integer\n")
	b.WriteString("#   bool      true / false\n")
	b.WriteString("#   duration  Go syntax: \"500ms\", \"30s\", \"2m\", \"1h\" (0 = disabled/unlimited\n")
	b.WriteString("#             where the key notes it)\n")
	b.WriteString("#   list      [item, …]   map   key: value\n")
	b.WriteString("#\n")
	b.WriteString("# Clients normally supply upstream URLs and credentials per request through\n")
	b.WriteString("# X-Proxy-* headers. Optional provider field maps and wire headers live below.\n")
	b.WriteString("# Configured headers can contain secrets: keep runtime configuration private.\n")
	b.WriteString("\n")

	byCat := map[string][]Field{}
	for _, f := range Schema() {
		byCat[f.Category] = append(byCat[f.Category], f)
	}
	for _, cat := range Categories() {
		fields := byCat[cat.ID]
		if len(fields) == 0 {
			continue
		}
		b.WriteString("# ---- " + cat.Label + " ----\n")
		if cat.Help != "" {
			writeCommentWrap(&b, cat.Help)
		}
		for _, f := range fields {
			if f.Help != "" {
				writeCommentWrap(&b, f.Help)
			}
			reload := "yes"
			if !f.HotReload {
				reload = "NO (restart required)"
			}
			b.WriteString("# Hot-reload: " + reload + ".\n")
			b.WriteString("#   " + f.TypeLine(defaults[f.Key]) + "\n")
			if err := writeKey(&b, f, values[f.Key]); err != nil {
				return err
			}
			b.WriteString("\n")
		}
	}
	// Separate fields with blank lines, but keep the generated document's EOF
	// canonical for source-control whitespace checks.
	_, err := io.WriteString(w, strings.TrimSuffix(b.String(), "\n"))
	return err
}

func writeCommentWrap(b *strings.Builder, s string) {
	for _, line := range wrapWords(s, 88) {
		b.WriteString("# " + line + "\n")
	}
}

func wrapWords(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	var cur strings.Builder
	for _, w := range words {
		if cur.Len() == 0 {
			cur.WriteString(w)
			continue
		}
		if cur.Len()+1+len(w) > width {
			lines = append(lines, cur.String())
			cur.Reset()
			cur.WriteString(w)
			continue
		}
		cur.WriteByte(' ')
		cur.WriteString(w)
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}

func writeKey(b *strings.Builder, f Field, v any) error {
	switch f.Kind {
	case KindProviders:
		m, _ := v.(map[string]ProviderOverride)
		if len(m) == 0 {
			b.WriteString(f.Key + ": {}\n")
			return nil
		}
		b.WriteString(f.Key + ":\n")
		// Stable order: Go map iteration is random; sort via a slice of keys.
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, label := range keys {
			p := m[label]
			b.WriteString("  " + yamlQuote(label) + ":\n")
			if len(p.CostKeys) == 0 {
				b.WriteString("    cost_keys: []\n")
			} else {
				b.WriteString("    cost_keys:\n")
				for _, k := range p.CostKeys {
					b.WriteString("      - " + yamlQuote(k) + "\n")
				}
			}
			if len(p.UsageKeys) == 0 {
				b.WriteString("    usage_keys: {}\n")
			} else {
				b.WriteString("    usage_keys:\n")
				uks := make([]string, 0, len(p.UsageKeys))
				for k := range p.UsageKeys {
					uks = append(uks, k)
				}
				sort.Strings(uks)
				for _, k := range uks {
					b.WriteString("      " + yamlQuote(k) + ": " + yamlQuote(p.UsageKeys[k]) + "\n")
				}
			}
			b.WriteString("    models_path: " + yamlQuote(p.ModelsPath) + "\n")
			if len(p.ModelsKeys) == 0 {
				b.WriteString("    models_keys: {}\n")
			} else {
				b.WriteString("    models_keys:\n")
				mks := make([]string, 0, len(p.ModelsKeys))
				for k := range p.ModelsKeys {
					mks = append(mks, k)
				}
				sort.Strings(mks)
				for _, k := range mks {
					b.WriteString("      " + yamlQuote(k) + ": " + yamlQuote(p.ModelsKeys[k]) + "\n")
				}
			}
			if len(p.Headers) == 0 {
				b.WriteString("    headers: {}\n")
			} else {
				b.WriteString("    headers:\n")
				hks := make([]string, 0, len(p.Headers))
				for k := range p.Headers {
					hks = append(hks, k)
				}
				sort.Strings(hks)
				for _, k := range hks {
					b.WriteString("      " + yamlQuote(k) + ": " + yamlQuote(p.Headers[k]) + "\n")
				}
			}
		}
		return nil
	case KindAliases:
		m, _ := v.(map[string]string)
		if len(m) == 0 {
			b.WriteString(f.Key + ": {}\n")
			return nil
		}
		b.WriteString(f.Key + ":\n")
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, from := range keys {
			b.WriteString("  " + yamlQuote(from) + ": " + yamlQuote(m[from]) + "\n")
		}
		return nil
	case KindStrings:
		ss, _ := v.([]string)
		if len(ss) == 0 {
			b.WriteString(f.Key + ": []\n")
			return nil
		}
		b.WriteString(f.Key + ":\n")
		for _, s := range ss {
			b.WriteString("  - " + yamlQuote(s) + "\n")
		}
		return nil
	case KindModelRules:
		rs, _ := v.([]ModelRule)
		if len(rs) == 0 {
			b.WriteString(f.Key + ": []\n")
			return nil
		}
		b.WriteString(f.Key + ":\n")
		for _, r := range rs {
			b.WriteString("  - {mode: " + r.Mode + ", from: " + yamlQuote(r.From) + ", to: " + yamlQuote(r.To))
			if r.Disabled {
				b.WriteString(", disabled: true")
			}
			b.WriteString("}\n")
		}
		return nil
	case KindBool:
		b.WriteString(f.Key + ": " + strconv.FormatBool(v.(bool)) + "\n")
		return nil
	case KindDuration:
		s, _ := v.(string)
		if s == "" {
			s = FormatDuration(0)
		}
		b.WriteString(f.Key + ": " + s + "\n")
		return nil
	case KindString:
		s, _ := v.(string)
		b.WriteString(f.Key + ": " + yamlQuote(s) + "\n")
		return nil
	case KindInt, KindBytes:
		b.WriteString(f.Key + ": " + fmt.Sprint(v) + "\n")
		return nil
	default:
		return fmt.Errorf("write %s: unsupported kind %s", f.Key, f.Kind)
	}
}

func yamlQuote(s string) string {
	// All strings are quoted: YAML's plain-scalar resolver otherwise turns
	// "null" into null and trims trailing spaces, silently changing settings.
	return strconv.Quote(s)
}
