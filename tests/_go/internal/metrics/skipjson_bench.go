package metrics

import (
	"strconv"
	"strings"
	"testing"
)

// skipJSONStringBenchShapes builds the escaped and plain string bodies the
// scanner is measured on. The escaped shape carries one backslash escape every
// five bytes, the density a JSON content payload shows after a codec escape;
// "tail" places a single escape before a long plain run.
func skipJSONStringBenchShapes(size int) map[string][]byte {
	return map[string][]byte{
		"plain":   []byte(`"` + strings.Repeat("x", size) + `"`),
		"escaped": []byte(`"` + strings.Repeat(`text\n`, size/6) + `"`),
		"tail":    []byte(`"` + `\n` + strings.Repeat("x", size) + `"`),
	}
}

// BenchmarkSkipJSONString measures the string terminator scan on a plain and on
// escaped content. Escaped content is what a codec-escaped body reaches, and it
// is the shape where any escape used to restart the byte loop from the start.
func BenchmarkSkipJSONString(b *testing.B) {
	for _, size := range []int{100 * 1024, 1024 * 1024} {
		shapes := skipJSONStringBenchShapes(size)
		for _, name := range []string{"plain", "escaped", "tail"} {
			body := shapes[name]
			b.Run(name+"/"+strconv.Itoa(size), func(b *testing.B) {
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				for b.Loop() {
					if got := SkipJSONString(body, 0); got != len(body) {
						b.Fatalf("SkipJSONString = %d, want %d", got, len(body))
					}
				}
			})
		}
	}
}
