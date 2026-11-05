package main

// Human-readable output. Standard library only; shared with the upstream
// contribution. The TUI has its own richer renderer in internal/tui.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

var colorOn = os.Getenv("NO_COLOR") == "" && os.Getenv("MVCTL_NO_COLOR") == ""

func setColor(on bool)   { colorOn = on }
func colorEnabled() bool { return colorOn }

func paint(s string, codes ...string) string {
	if !colorOn || len(codes) == 0 {
		return s
	}
	return "\x1b[" + strings.Join(codes, ";") + "m" + s + "\x1b[0m"
}

func bold(s string) string    { return paint(s, "1") }
func dim(s string) string     { return paint(s, "2") }
func red(s string) string     { return paint(s, "31") }
func green(s string) string   { return paint(s, "32") }
func yellow(s string) string  { return paint(s, "33") }
func blue(s string) string    { return paint(s, "34") }
func magenta(s string) string { return paint(s, "35") }
func cyan(s string) string    { return paint(s, "36") }
func gray(s string) string    { return paint(s, "90") }

func strip(s string) string { return ansiRe.ReplaceAllString(s, "") }
func width(s string) int    { return utf8.RuneCountInString(strip(s)) }

func pad(s string, n int, right bool) string {
	gap := n - width(s)
	if gap < 0 {
		gap = 0
	}
	if right {
		return strings.Repeat(" ", gap) + s
	}
	return s + strings.Repeat(" ", gap)
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}

func num(n float64) string {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return "—"
	}
	a := math.Abs(n)
	switch {
	case a >= 1e12:
		return fmt.Sprintf("%.2fT", n/1e12)
	case a >= 1e9:
		return fmt.Sprintf("%.2fB", n/1e9)
	case a >= 1e6:
		return fmt.Sprintf("%.2fM", n/1e6)
	case a >= 1e4:
		return fmt.Sprintf("%.1fk", n/1e3)
	case n == math.Trunc(n):
		return strconv.FormatInt(int64(n), 10)
	default:
		return fmt.Sprintf("%.2f", n)
	}
}

func cost(n float64) string {
	if n >= 100 {
		return fmt.Sprintf("$%.2f", n)
	}
	if n >= 1 {
		return fmt.Sprintf("$%.3f", n)
	}
	return fmt.Sprintf("$%.4f", n)
}

func dur(ms float64) string {
	if ms <= 0 {
		return "—"
	}
	if ms < 1000 {
		return fmt.Sprintf("%.0fms", ms)
	}
	if ms < 60_000 {
		return fmt.Sprintf("%.1fs", ms/1000)
	}
	if ms < 3_600_000 {
		return fmt.Sprintf("%.1fm", ms/60_000)
	}
	return fmt.Sprintf("%.1fh", ms/3_600_000)
}

func bytesHuman(n float64) string {
	if n <= 0 {
		return "0B"
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if n >= 10 || i == 0 {
		return fmt.Sprintf("%.0f%s", n, units[i])
	}
	return fmt.Sprintf("%.1f%s", n, units[i])
}

func shortTime(v string) string {
	if v == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return truncate(v, 19)
	}
	return t.Format("15:04:05")
}

func statusCode(code int) string {
	s := fmt.Sprintf("%d", code)
	switch {
	case code == 0:
		return red("ERR")
	case code < 300:
		return green(s)
	case code < 400:
		return cyan(s)
	case code < 500:
		return yellow(s)
	default:
		return red(s)
	}
}

func heading(title string) string {
	return bold(cyan(title)) + "\n" + dim(strings.Repeat("─", min(60, len(title)+8)))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func kv(pairs [][2]string) string {
	w := 0
	for _, p := range pairs {
		if len(p[0]) > w {
			w = len(p[0])
		}
	}
	var b strings.Builder
	for _, p := range pairs {
		b.WriteString(dim(pad(p[0], w+2, false)))
		b.WriteString(p[1])
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func table(head []string, rows [][]string) string {
	cols := len(head)
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	widths := make([]int, cols)
	all := rows
	if len(head) > 0 {
		all = append([][]string{head}, rows...)
	}
	for _, r := range all {
		for i, cell := range r {
			if i < cols && width(cell) > widths[i] {
				widths[i] = width(cell)
			}
		}
	}
	format := func(r []string, isHead bool) string {
		var b strings.Builder
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(r) {
				cell = r[i]
			}
			s := pad(cell, widths[i], false)
			if isHead {
				s = dim(bold(s))
			}
			b.WriteString(s)
			if i < cols-1 {
				b.WriteString("  ")
			}
		}
		return strings.TrimRight(b.String(), " ")
	}
	var out []string
	if len(head) > 0 {
		out = append(out, format(head, true))
	}
	for _, r := range rows {
		out = append(out, format(r, false))
	}
	return strings.Join(out, "\n")
}

func spark(values []float64, w int) string {
	if w < 1 {
		w = 1
	}
	if len(values) == 0 {
		return strings.Repeat("▁", w)
	}
	blocks := []rune("▁▂▃▄▅▆▇█")
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range values {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	if hi == lo {
		hi = lo + 1
	}
	// Linear resampling: every output bucket maps to one source sample, so a
	// short series never produces artificial zero-height troughs.
	var b strings.Builder
	for i := 0; i < w; i++ {
		idx := i * len(values) / w
		if idx >= len(values) {
			idx = len(values) - 1
		}
		v := values[idx]
		f := (v - lo) / (hi - lo)
		if f < 0 {
			f = 0
		}
		if f > 1 {
			f = 1
		}
		level := int(f * float64(len(blocks)-1))
		b.WriteRune(blocks[level])
	}
	return cyan(b.String())
}

func bar(label string, value, max float64, w int) string {
	if w < 1 {
		w = 1
	}
	if max <= 0 {
		max = 1
	}
	f := value / max
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	n := int(f * float64(w))
	return fmt.Sprintf("%s %s %s", pad(truncate(label, 18), 18, false),
		cyan(strings.Repeat("█", n)+dim(strings.Repeat("░", w-n))), num(value))
}

// redact removes the operator credential from anything we print as JSON.
// redact removes credentials from anything marshalled as JSON. It reflects
// over arbitrary structs, typed maps and slices, so a typed value such as
// map[string]config.Profile can never smuggle a token past the type switch.
func redact(v any) any {
	return redactValue(reflect.ValueOf(v))
}

func redactValue(rv reflect.Value) any {
	if !rv.IsValid() {
		return nil
	}
	switch rv.Kind() {
	case reflect.Interface, reflect.Pointer:
		if rv.IsNil() {
			return nil
		}
		return redactValue(rv.Elem())
	case reflect.Map:
		out := map[string]any{}
		iter := rv.MapRange()
		for iter.Next() {
			k := fmt.Sprint(iter.Key().Interface())
			if sensitiveKey(k) {
				out[k] = redactedValue(iter.Value())
				continue
			}
			out[k] = redactValue(iter.Value())
		}
		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = redactValue(rv.Index(i))
		}
		return out
	case reflect.Struct:
		out := map[string]any{}
		rt := rv.Type()
		for i := 0; i < rv.NumField(); i++ {
			field := rt.Field(i)
			if field.PkgPath != "" { // unexported
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			if sensitiveKey(name) {
				out[name] = redactedValue(rv.Field(i))
				continue
			}
			out[name] = redactValue(rv.Field(i))
		}
		return out
	case reflect.String:
		return rv.String()
	default:
		return rv.Interface()
	}
}

func redactedValue(rv reflect.Value) any {
	if !rv.IsValid() {
		return nil
	}
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.String && rv.Len() > 0 {
		return "[redacted]"
	}
	return nil
}

func sensitiveKey(k string) bool {
	lower := strings.ToLower(k)
	for _, needle := range []string{
		"token", "secret", "password", "authorization", "bearer", "credential",
		"api_key", "apikey", "auth", "key_hash",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func toJSON(v any) string {
	b, err := json.MarshalIndent(redact(v), "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}
