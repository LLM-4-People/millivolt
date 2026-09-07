package config

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// byteSizeUnits is the single suffix table for parse and format. Longest
// prefix first so MiB is not consumed as B.
var byteSizeUnits = []struct {
	name string
	size int64
}{
	{"GiB", 1 << 30},
	{"MiB", 1 << 20},
	{"KiB", 1 << 10},
	{"B", 1},
}

// ByteSize is a byte budget. YAML, Settings and GET /admin/config use the same
// magnitude-plus-unit grammar as durations. A bare integer is still accepted
// as a count of bytes so existing files load.
type ByteSize int64

func (b *ByteSize) UnmarshalYAML(value *yaml.Node) error {
	if value == nil || value.Kind != yaml.ScalarNode {
		return errByteSize()
	}
	n, err := parseByteSizeString(value.Value)
	if err != nil {
		return err
	}
	*b = ByteSize(n)
	return nil
}

func errByteSize() error {
	return fmt.Errorf("want a byte size (e.g. %s, %s)", FormatByteSize(int64(Default().MaxRequestBytes)), FormatByteSize(DebugCaptureMaxBytesMin))
}

func checkByteSize(key string, got ByteSize, min, max int64) error {
	n := int64(got)
	if n < min || n > max {
		return fmt.Errorf("%s: must be %s..%s, got %s", key, FormatByteSize(min), FormatByteSize(max), FormatByteSize(n))
	}
	return nil
}

// FormatByteSize renders a byte budget the way proxy.yaml writes it. Coarsest
// exact unit from byteSizeUnits wins. Values that are not a whole KiB/MiB/GiB
// keep an explicit B suffix so they are never unitless integers.
func FormatByteSize(n int64) string {
	if n < 0 {
		return "-" + FormatByteSize(-n)
	}
	if n == 0 {
		return "0" + byteSizeUnits[len(byteSizeUnits)-1].name
	}
	for _, u := range byteSizeUnits {
		if u.size > 1 && n%u.size == 0 {
			return strconv.FormatInt(n/u.size, 10) + u.name
		}
	}
	return strconv.FormatInt(n, 10) + byteSizeUnits[len(byteSizeUnits)-1].name
}

func parseByteSize(v any) (int64, error) {
	switch t := v.(type) {
	case ByteSize:
		return int64(t), nil
	case int:
		return int64(t), nil
	case int64:
		return t, nil
	case uint64:
		if t > uint64(^uint64(0)>>1) {
			return 0, errByteSize()
		}
		return int64(t), nil
	case float64:
		n := int64(t)
		if float64(n) != t {
			return 0, fmt.Errorf("want a whole byte size, got %v", t)
		}
		return n, nil
	case string:
		return parseByteSizeString(t)
	default:
		return 0, fmt.Errorf("%w, got %T", errByteSize(), v)
	}
}

func parseByteSizeString(s string) (int64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	if s == "" {
		return 0, errByteSize()
	}
	upper := strings.ToUpper(s)
	mult := int64(1)
	for _, u := range byteSizeUnits {
		suf := strings.ToUpper(u.name)
		if strings.HasSuffix(upper, suf) {
			if u.name == "B" && strings.HasSuffix(upper, "IB") {
				continue
			}
			mult = u.size
			upper = strings.TrimSuffix(upper, suf)
			break
		}
	}
	n, err := strconv.ParseInt(upper, 10, 64)
	if err != nil || n < 0 {
		return 0, errByteSize()
	}
	if mult != 1 && n > (1<<63-1)/mult {
		return 0, errByteSize()
	}
	return n * mult, nil
}
