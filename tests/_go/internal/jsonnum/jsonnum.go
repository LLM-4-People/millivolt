package jsonnum

// Round 7 verifier D1: one owner decides which JSON number spellings are
// integral. These cases pin the exact grammar boundaries shared by the proxy
// trust boundary and the three wire decoders.

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
	"testing"
)

func TestParseIntegralSpellings(t *testing.T) {
	for _, tc := range []struct {
		literal string
		want    int
	}{
		{"0", 0},
		{"-0", 0},
		{"0.0", 0},
		{"0e999", 0},
		{"1", 1},
		{"-1", -1},
		{"1000000", 1000000},
		{"1e6", 1000000},
		{"1E+6", 1000000},
		{"1000000.0", 1000000},
		{"1000000.000", 1000000},
		{"10000e2", 1000000},
		{"-1000000", -1000000},
	} {
		t.Run(tc.literal, func(t *testing.T) {
			got, err := Parse([]byte(tc.literal))
			if err != nil {
				t.Fatalf("Parse(%s) error = %v", tc.literal, err)
			}
			if got != tc.want {
				t.Fatalf("Parse(%s) = %d, want %d", tc.literal, got, tc.want)
			}
		})
	}
}

func TestParseRejectsNonIntegralValues(t *testing.T) {
	for _, tc := range []struct {
		literal string
		want    error
	}{
		{"", ErrNotNumber},
		{"null", ErrNotNumber},
		{"true", ErrNotNumber},
		{`"1000000"`, ErrNotNumber},
		{"1e", ErrNotNumber},
		{"1.", ErrNotNumber},
		{".5", ErrNotNumber},
		{"1.5", ErrNotInteger},
		{"0.5", ErrNotInteger},
		{"1e-1", ErrNotInteger},
		{"1.0000001", ErrNotInteger},
		{"99999999999999999999", ErrOutOfRange},
		{"1e99", ErrOutOfRange},
		{"1e999999999999999999999", ErrOutOfRange},
		{"-99999999999999999999", ErrOutOfRange},
	} {
		t.Run(tc.literal, func(t *testing.T) {
			if _, err := Parse([]byte(tc.literal)); !errors.Is(err, tc.want) {
				t.Fatalf("Parse(%s) error = %v, want %v", tc.literal, err, tc.want)
			}
		})
	}
}

func TestOptionalIntPresence(t *testing.T) {
	if got, err := OptionalInt(nil, "max_tokens"); err != nil || got != nil {
		t.Fatalf("absent member = %v, %v; want nil, nil", got, err)
	}
	if got, err := OptionalInt(json.RawMessage("null"), "max_tokens"); err != nil || got != nil {
		t.Fatalf("null member = %v, %v; want nil, nil", got, err)
	}
	got, err := OptionalInt(json.RawMessage("1e6"), "max_tokens")
	if err != nil || got == nil || *got != 1000000 {
		t.Fatalf("1e6 member = %v, %v; want 1000000", got, err)
	}
	if _, err := OptionalInt(json.RawMessage("1.5"), "max_tokens"); err == nil ||
		!strings.Contains(err.Error(), "max_tokens") || !strings.Contains(err.Error(), "must be an integer") {
		t.Fatalf("fractional member error = %v, want the field-named rejection", err)
	}
}

// TestParseRejectsLeadingZero pins the JSON number grammar at the one place
// that accepts a literal character by character: an integer part may be a
// single zero, never a zero followed by more digits. Zero with a fraction or
// exponent stays valid.
func TestParseRejectsLeadingZero(t *testing.T) {
	for _, literal := range []string{"00", "01", "-00", "-01", "00.1", "01e2", "-00e1"} {
		t.Run(literal, func(t *testing.T) {
			var num json.Number
			if json.Unmarshal([]byte(literal), &num) == nil {
				t.Fatalf("encoding/json accepted %q as a number; fixture is invalid", literal)
			}
			if got, err := Parse([]byte(literal)); !errors.Is(err, ErrNotNumber) {
				t.Fatalf("Parse(%s) = %d, %v; want ErrNotNumber", literal, got, err)
			}
		})
	}
	for _, tc := range []struct {
		literal string
		want    int
	}{
		{"0", 0}, {"-0", 0}, {"0.0", 0}, {"0e1", 0}, {"-0.000", 0},
	} {
		t.Run("valid "+tc.literal, func(t *testing.T) {
			got, err := Parse([]byte(tc.literal))
			if err != nil || got != tc.want {
				t.Fatalf("Parse(%s) = %d, %v; want %d", tc.literal, got, err, tc.want)
			}
		})
	}
}

// jsonNumberLiteral reports whether the trimmed input is a valid JSON number,
// the grammar Parse must follow. json.Unmarshal into json.Number is not a
// grammar oracle: it accepts null and string values for that Go type.
func jsonNumberLiteral(literal string) bool {
	trimmed := strings.TrimSpace(literal)
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return false
	}
	first := trimmed[0]
	return first == '-' || (first >= '0' && first <= '9')
}

// literalRat builds an exact decimal rational for a JSON number whose exponent
// is small enough to materialize. It is the independent oracle for Parse.
func literalRat(literal string) (*big.Rat, bool) {
	lit := strings.TrimSpace(literal)
	mantissa := lit
	exp := 0
	if i := strings.IndexAny(lit, "eE"); i >= 0 {
		parsed, err := strconv.Atoi(lit[i+1:])
		if err != nil || parsed < -1000 || parsed > 1000 {
			return nil, false
		}
		mantissa, exp = lit[:i], parsed
	}
	r, ok := new(big.Rat).SetString(mantissa)
	if !ok {
		return nil, false
	}
	if exp != 0 {
		pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(absInt(exp))), nil)
		if exp > 0 {
			r.Mul(r, new(big.Rat).SetInt(pow))
		} else {
			r.Quo(r, new(big.Rat).SetInt(pow))
		}
	}
	return r, true
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// TestParseMatchesRatOracle checks the accept/reject verdict and the exact
// value against an independent big.Rat decimal parser. The table mixes valid,
// fractional, overflowing and non-number spellings.
func TestParseMatchesRatOracle(t *testing.T) {
	for _, literal := range []string{
		"0", "-0", "0.0", "0e1", "-0.000", "1", "-1", "10", "123",
		"1e6", "1E+6", "1000000.0", "1000000.000", "10000e2", "1.2300e2",
		"123.000", "-1000000", "1e-1", "0.5", "1.0000001", "1e99",
		"9223372036854775807", "9223372036854775808", "-9223372036854775808",
		"00", "01", "-01", "1.", ".5", "1e", "null", "true", `"1"`,
	} {
		t.Run(literal, func(t *testing.T) {
			grammarOK := jsonNumberLiteral(literal)
			got, err := Parse([]byte(literal))
			if !grammarOK {
				if !errors.Is(err, ErrNotNumber) {
					t.Fatalf("Parse(%s) = %d, %v; want ErrNotNumber", literal, got, err)
				}
				return
			}
			if errors.Is(err, ErrNotNumber) {
				t.Fatalf("Parse(%s) rejected valid JSON number as not a number", literal)
			}
			r, ok := literalRat(literal)
			if !ok {
				t.Fatalf("oracle could not parse valid number %s", literal)
			}
			switch {
			case !r.IsInt():
				if !errors.Is(err, ErrNotInteger) {
					t.Fatalf("Parse(%s) = %d, %v; want ErrNotInteger", literal, got, err)
				}
			case !r.Num().IsInt64() || int64(int(r.Num().Int64())) != r.Num().Int64():
				if !errors.Is(err, ErrOutOfRange) {
					t.Fatalf("Parse(%s) = %d, %v; want ErrOutOfRange", literal, got, err)
				}
			default:
				if err != nil {
					t.Fatalf("Parse(%s) error = %v, want %d", literal, err, r.Num().Int64())
				}
				if big.NewInt(int64(got)).Cmp(r.Num()) != 0 {
					t.Fatalf("Parse(%s) = %d, want %s", literal, got, r.Num())
				}
			}
		})
	}
}

// FuzzParseNumberGrammar checks that Parse accepts a literal only when
// encoding/json recognizes a JSON number and that an accepted value matches
// the rational oracle. The seeds cover the leading-zero regression and the
// valid, fractional and overflow boundaries.
func FuzzParseNumberGrammar(f *testing.F) {
	for _, seed := range []string{
		"0", "-0", "0.0", "0e1", "1", "1e6", "1000000.0", "123.000",
		"1.5", "1e-1", "1e99", "9223372036854775808",
		"00", "01", "-01", "", "null", "1.", ".5", "1e",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, literal string) {
		if len(literal) > 64 {
			return
		}
		grammarOK := jsonNumberLiteral(literal)
		got, err := Parse([]byte(literal))
		if err != nil {
			if grammarOK && errors.Is(err, ErrNotNumber) {
				t.Fatalf("valid JSON number %q rejected as not a number", literal)
			}
			return
		}
		if !grammarOK {
			t.Fatalf("Parse(%q) = %d accepted a non-number literal", literal, got)
		}
		r, ok := literalRat(literal)
		if !ok {
			// A zero literal with a huge exponent is valid and integral; its
			// rational has no materializable denominator, so skip the oracle.
			if got != 0 {
				t.Fatalf("Parse(%q) = %d without an oracle", literal, got)
			}
			return
		}
		if !r.IsInt() || !r.Num().IsInt64() || int64(got) != r.Num().Int64() {
			t.Fatalf("Parse(%q) = %d, want the exact rational %s", literal, got, r)
		}
	})
}

func TestParseInt64Bounds(t *testing.T) {
	for _, tc := range []struct {
		literal string
		want    int64
		wantErr error
	}{
		{literal: "0", want: 0},
		{literal: "5", want: 5},
		{literal: "5.0", want: 5},
		{literal: "5e0", want: 5},
		{literal: "1e3", want: 1000},
		{literal: "-2", want: -2},
		{literal: "1000000000000000000", want: 1000000000000000000},
		{literal: "1e18", want: 1000000000000000000},
		{literal: "9223372036854775807", want: 9223372036854775807},
		{literal: "-9223372036854775808", want: -9223372036854775808},
		{literal: "9223372036854775808", wantErr: ErrOutOfRange},
		{literal: "5.5", wantErr: ErrNotInteger},
		{literal: `"5"`, wantErr: ErrNotNumber},
		{literal: "null", wantErr: ErrNotNumber},
	} {
		t.Run(tc.literal, func(t *testing.T) {
			got, err := ParseInt64([]byte(tc.literal))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseInt64(%s) error = %v, want %v", tc.literal, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ParseInt64(%s) = %d, %v; want %d", tc.literal, got, err, tc.want)
			}
		})
	}
}

func TestParseIntBounds(t *testing.T) {
	min := big.NewInt(int64(math.MinInt))
	max := big.NewInt(int64(math.MaxInt))
	above := new(big.Int).Add(max, big.NewInt(1)).String()
	below := new(big.Int).Sub(min, big.NewInt(1)).String()
	for _, tc := range []struct {
		literal string
		want    int
		wantErr error
	}{
		{literal: max.String(), want: math.MaxInt},
		{literal: min.String(), want: math.MinInt},
		{literal: above, wantErr: ErrOutOfRange},
		{literal: below, wantErr: ErrOutOfRange},
	} {
		t.Run(tc.literal, func(t *testing.T) {
			got, err := Parse([]byte(tc.literal))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Parse(%s) error = %v, want %v", tc.literal, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Parse(%s) = %d, %v; want %d", tc.literal, got, err, tc.want)
			}
		})
	}
}

func TestInt64Unmarshal(t *testing.T) {
	for _, tc := range []struct {
		literal string
		want    int64
		wantErr bool
	}{
		{"5", 5, false},
		{"5.0", 5, false},
		{"1e3", 1000, false},
		{"0", 0, false},
		{"-2", -2, false},
		{"5.5", 0, true},
		{`"5"`, 0, true},
		{"null", 0, true},
	} {
		t.Run(tc.literal, func(t *testing.T) {
			var got Int64
			err := json.Unmarshal([]byte(tc.literal), &got)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Unmarshal(%s) error = %v, wantErr %v", tc.literal, err, tc.wantErr)
			}
			if !tc.wantErr && int64(got) != tc.want {
				t.Fatalf("Unmarshal(%s) = %d, want %d", tc.literal, got, tc.want)
			}
		})
	}
}

func TestUnixSecondsUnmarshal(t *testing.T) {
	for _, tc := range []struct {
		literal string
		want    int64
		wantErr error
	}{
		{literal: "1.5", want: 1},
		{literal: "1.999", want: 1},
		{literal: "1698982736.0", want: 1698982736},
		{literal: "0.999", want: 0},
		{literal: "-0", want: 0},
		{literal: "-0.0", want: 0},
		{literal: "0", want: 0},
		{literal: "1e3", want: 1000},
		{literal: "-0.5", wantErr: ErrNegative},
		{literal: "-1", wantErr: ErrNegative},
		{literal: "-1e3", wantErr: ErrNegative},
		{literal: "1e20", wantErr: ErrOutOfRange},
		{literal: `"1"`, wantErr: ErrNotNumber},
		{literal: "null", wantErr: ErrNotNumber},
	} {
		t.Run(tc.literal, func(t *testing.T) {
			var got UnixSeconds
			err := json.Unmarshal([]byte(tc.literal), &got)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Unmarshal(%s) error = %v, want %v", tc.literal, err, tc.wantErr)
				}
				return
			}
			if err != nil || int64(got) != tc.want {
				t.Fatalf("Unmarshal(%s) = %d, %v; want %d", tc.literal, got, err, tc.want)
			}
		})
	}
}
