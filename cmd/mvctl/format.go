package main

// Human-readable rendering. Every cell that comes from the proxy passes
// through sanitizeCell before it reaches a terminal: request labels, model
// ids and error types are attacker-influenced, and a raw ESC would let one
// operator command rewrite another line of the terminal.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

// Styling is emitted as private-use sentinels, not raw ESC, so sanitizeCell can
// remove untrusted control sequences without stripping the command's own
// colors. render is the only place that turns a sentinel into an SGR sequence.
// The markers carry a per-process nonce, so a captured value cannot forge one
// and turn its own text into an escape sequence.
const (
	styleStart = '\uE000'
	styleEnd   = '\uE001'
)

var (
	styleNonce   = newStyleNonce()
	styleStartRe = regexp.MustCompile(regexp.QuoteMeta(string(styleStart)+styleNonce) + `[0-9;]*m`)
	styleEndMark = string(styleEnd) + styleNonce
)

func newStyleNonce() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// Without a nonce paint cannot distinguish its own markers from
		// untrusted text, so it stays off rather than weakening sanitization.
		return ""
	}
	return hex.EncodeToString(raw[:])
}

var colorOn bool

func setColor(on bool) { colorOn = on }

func paint(s string, codes ...string) string {
	if !colorOn || len(codes) == 0 || styleNonce == "" {
		return s
	}
	return string(styleStart) + styleNonce + strings.Join(codes, ";") + "m" + s + styleEndMark
}

// render converts the internal style sentinels to SGR sequences at the one
// point text leaves for a terminal. A marker without this process's nonce is
// stripped, never interpreted, so untrusted text cannot become an escape.
func render(s string) string {
	if styleNonce == "" {
		return stripSentinelRunes(s)
	}
	s = styleStartRe.ReplaceAllStringFunc(s, func(match string) string {
		return "\x1b[" + match[len(string(styleStart))+len(styleNonce):]
	})
	s = strings.ReplaceAll(s, styleEndMark, "\x1b[0m")
	return stripSentinelRunes(s)
}

func stripSentinelRunes(s string) string {
	return strings.Map(func(r rune) rune {
		if r == styleStart || r == styleEnd {
			return -1
		}
		return r
	}, s)
}

// stripForeignStyles removes any sentinel that does not carry this process's
// nonce while preserving the markers paint emitted.
func stripForeignStyles(s string) string {
	if styleNonce == "" {
		return stripSentinelRunes(s)
	}
	var out strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r != styleStart && r != styleEnd {
			out.WriteString(s[i : i+size])
			i += size
			continue
		}
		rest := s[i+size:]
		if strings.HasPrefix(rest, styleNonce) {
			out.WriteString(s[i : i+size+len(styleNonce)])
			i += size + len(styleNonce)
			continue
		}
		i += size
	}
	return out.String()
}

func bold(s string) string   { return paint(s, "1") }
func dim(s string) string    { return paint(s, "2") }
func red(s string) string    { return paint(s, "31") }
func green(s string) string  { return paint(s, "32") }
func yellow(s string) string { return paint(s, "33") }
func cyan(s string) string   { return paint(s, "36") }

func strip(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = styleStartRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, styleEndMark, "")
	return stripSentinelRunes(s)
}
func width(s string) int { return utf8.RuneCountInString(strip(s)) }

// sanitizeCell removes terminal control sequences and C0 controls from data
// the proxy or a client supplied. Tabs and newlines become spaces so a table
// stays one row per record.
func sanitizeCell(s string) string {
	s = stripForeignStyles(s)
	s = ansiRe.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, s)
}

// sanitizeText keeps line structure for text surfaces (the Prometheus
// exposition) while removing terminal escapes and other C0 controls.
func sanitizeText(s string) string {
	s = strip(s)
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, s)
}

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
	return string(runes[:n-1]) + "..."
}

func num(n float64) string {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return "n/a"
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

func numPtr(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return num(*v)
}

func cost(n float64) string {
	switch {
	case n >= 100:
		return fmt.Sprintf("$%.2f", n)
	case n >= 1:
		return fmt.Sprintf("$%.3f", n)
	default:
		return fmt.Sprintf("$%.4f", n)
	}
}

func costPtr(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return cost(*v)
}

func dur(ms float64) string {
	if math.IsNaN(ms) || math.IsInf(ms, 0) || ms < 0 {
		return "n/a"
	}
	if ms == 0 {
		return "0ms"
	}
	switch {
	case ms < 1000:
		return fmt.Sprintf("%.0fms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", ms/1000)
	case ms < 3_600_000:
		return fmt.Sprintf("%.1fm", ms/60_000)
	default:
		return fmt.Sprintf("%.1fh", ms/3_600_000)
	}
}

func durPtr(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return dur(*v)
}

func shortTime(t time.Time) string {
	if t.IsZero() {
		return "n/a"
	}
	return t.Local().Format("15:04:05")
}

func statusCode(code int) string {
	s := strconv.Itoa(code)
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
	title = sanitizeCell(title)
	return bold(cyan(title)) + "\n" + dim(strings.Repeat("-", min(60, width(title)+8)))
}

func kv(pairs [][2]string) string {
	w := 0
	for _, pair := range pairs {
		if l := width(sanitizeCell(pair[0])); l > w {
			w = l
		}
	}
	var b strings.Builder
	for _, pair := range pairs {
		b.WriteString(dim(pad(sanitizeCell(pair[0]), w+2, false)))
		b.WriteString(sanitizeCell(pair[1]))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func table(head []string, rows [][]string) string {
	cols := len(head)
	for _, row := range rows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	widths := make([]int, cols)
	all := rows
	if len(head) > 0 {
		all = append([][]string{head}, rows...)
	}
	for _, row := range all {
		for i, cell := range row {
			if i < cols {
				if l := width(sanitizeCell(cell)); l > widths[i] {
					widths[i] = l
				}
			}
		}
	}
	format := func(row []string, isHead bool) string {
		var b strings.Builder
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(row) {
				cell = sanitizeCell(row[i])
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
	for _, row := range rows {
		out = append(out, format(row, false))
	}
	return strings.Join(out, "\n")
}

func spark(values []float64, w int) string {
	if w < 1 {
		w = 1
	}
	if len(values) == 0 {
		return strings.Repeat("_", w)
	}
	blocks := []rune("_.:-=+*#")
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
	var b strings.Builder
	for i := 0; i < w; i++ {
		idx := i * len(values) / w
		if idx >= len(values) {
			idx = len(values) - 1
		}
		fraction := (values[idx] - lo) / (hi - lo)
		level := int(fraction * float64(len(blocks)-1))
		if level < 0 {
			level = 0
		}
		if level >= len(blocks) {
			level = len(blocks) - 1
		}
		b.WriteRune(blocks[level])
	}
	return cyan(b.String())
}

func toJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("{\"error\": %q}", err.Error())
	}
	return string(b)
}

// prettyJSON is the human-output form of a server document. The document may
// echo attacker-influenced request labels or error text, so terminal control
// sequences are removed before it reaches a TTY. Machine output keeps toJSON
// so a JSON consumer receives the value unchanged.
func prettyJSON(v any) string { return sanitizeText(toJSON(v)) }
