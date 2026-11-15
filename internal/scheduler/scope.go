package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// LimitScope selects the traffic one operator limit applies to. Every empty
// dimension matches any value; a non-empty dimension matches when the request
// value is a member. Dimensions intersect, so a request must satisfy every
// non-empty dimension. Values inside one dimension are alternatives. This is
// the same selector grammar operator pause holds and debug captures use, so
// an operator meets one scope vocabulary across the console.
//
// Values are compared byte-for-byte against the request's recorded identity:
// providers after configured aliases, models as the request spelling at
// admission, clients as the claimed or inferred label, key hashes as the
// SHA-256 digest, and conversations as the selected leaf id. A scope is
// operator input and therefore bounded and normalized at install time.
type LimitScope struct {
	Providers     []string `json:"providers,omitempty"`
	Models        []string `json:"models,omitempty"`
	Clients       []string `json:"clients,omitempty"`
	KeyHashes     []string `json:"key_hashes,omitempty"`
	Conversations []string `json:"conversations,omitempty"`
}

// LimitSchedule restricts a policy to a recurring local-time window. Days use
// time.Weekday numbering (0 = Sunday). StartMinute and EndMinute are minutes
// since local midnight; equal values mean all day. Start greater than end
// spans midnight, and the after-midnight portion belongs to the previous day.
// An empty schedule is always active. Local time is the server's configured
// timezone, the same clock the explorer uses for its day buckets.
type LimitSchedule struct {
	Days     []int `json:"days,omitempty"`
	StartMin int   `json:"start_minute,omitempty"`
	EndMin   int   `json:"end_minute,omitempty"`
}

const (
	minutesPerDay = 24 * 60
	// maxScopeValues bounds one dimension of one policy. Known vocabularies are
	// far smaller; the bound keeps a hand-written API body from growing the
	// persisted document without limit.
	maxScopeValues = 512
)

// NormalizeLimitScope trims, deduplicates and sorts every dimension. It is the
// single owner of scope canonicalization, so the persisted identity, the
// in-memory policy key and the UI echo can never disagree.
func NormalizeLimitScope(scope LimitScope) LimitScope {
	return LimitScope{
		Providers:     normalizeScopeValues(scope.Providers),
		Models:        normalizeScopeValues(scope.Models),
		Clients:       normalizeScopeValues(scope.Clients),
		KeyHashes:     normalizeScopeValues(scope.KeyHashes),
		Conversations: normalizeScopeValues(scope.Conversations),
	}
}

func normalizeScopeValues(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// Empty reports whether the scope matches every request.
func (s LimitScope) Empty() bool {
	return len(s.Providers) == 0 && len(s.Models) == 0 && len(s.Clients) == 0 &&
		len(s.KeyHashes) == 0 && len(s.Conversations) == 0
}

// ProviderOnly reports whether the scope is a single provider and nothing
// more. Anti-burst ramps and the legacy provider field are defined only for
// this shape.
func (s LimitScope) ProviderOnly() bool {
	return len(s.Providers) == 1 && len(s.Models) == 0 && len(s.Clients) == 0 &&
		len(s.KeyHashes) == 0 && len(s.Conversations) == 0
}

// PrimaryProvider returns the provider label for a single-provider scope and
// an empty string otherwise. It is the one bridge to the legacy provider API.
func (s LimitScope) PrimaryProvider() string {
	if s.ProviderOnly() {
		return s.Providers[0]
	}
	return ""
}

// Matches reports whether a request identity satisfies every non-empty
// dimension.
func (s LimitScope) Matches(provider, model, client, keyHash, conversation string) bool {
	return scopeHas(s.Providers, provider) && scopeHas(s.Models, model) &&
		scopeHas(s.Clients, client) && scopeHas(s.KeyHashes, keyHash) &&
		scopeHas(s.Conversations, conversation)
}

func scopeHas(values []string, value string) bool {
	if len(values) == 0 {
		return true
	}
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// PolicyID is the one owner of policy identity: a digest of the normalized
// scope plus the schedule. Two policies therefore differ exactly when their
// selector differs, and an edit that changes either replaces the policy rather
// than silently keeping counters under an old identity.
func PolicyID(scope LimitScope, schedule LimitSchedule) string {
	scope = NormalizeLimitScope(scope)
	schedule = NormalizeLimitSchedule(schedule)
	var b strings.Builder
	appendDimension(&b, "p", scope.Providers)
	appendDimension(&b, "m", scope.Models)
	appendDimension(&b, "c", scope.Clients)
	appendDimension(&b, "k", scope.KeyHashes)
	appendDimension(&b, "v", scope.Conversations)
	for _, day := range schedule.Days {
		b.WriteString("d")
		b.WriteString(string(rune('0' + day)))
	}
	if schedule.StartMin != schedule.EndMin || len(schedule.Days) > 0 {
		b.WriteString("s")
		b.WriteString(itoa(schedule.StartMin))
		b.WriteString("e")
		b.WriteString(itoa(schedule.EndMin))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:16])
}

func appendDimension(b *strings.Builder, name string, values []string) {
	if len(values) == 0 {
		return
	}
	b.WriteString(name)
	for _, value := range values {
		b.WriteString(itoa(len(value)))
		b.WriteString(":")
		b.WriteString(value)
	}
}

// itoa is the tiny non-negative decimal writer used by the identity builder.
// strconv would be correct but drags a dependency into a hot install path for
// one call site; the values are bounded by the API's numeric bands.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// NormalizeLimitSchedule clamps and sorts a schedule. It returns the zero
// schedule for an all-day every-day window so identity stays stable.
func NormalizeLimitSchedule(schedule LimitSchedule) LimitSchedule {
	days := make([]int, 0, len(schedule.Days))
	seen := make(map[int]struct{}, len(schedule.Days))
	for _, day := range schedule.Days {
		if day < 0 || day > 6 {
			continue
		}
		if _, ok := seen[day]; ok {
			continue
		}
		seen[day] = struct{}{}
		days = append(days, day)
	}
	sort.Ints(days)
	if len(days) == 7 {
		days = nil
	}
	start := schedule.StartMin
	end := schedule.EndMin
	if start < 0 || start >= minutesPerDay {
		start = 0
	}
	if end < 0 || end >= minutesPerDay {
		end = 0
	}
	if start == end && len(days) == 0 {
		return LimitSchedule{}
	}
	return LimitSchedule{Days: days, StartMin: start, EndMin: end}
}

// Empty reports whether the schedule is always active.
func (s LimitSchedule) Empty() bool {
	return len(s.Days) == 0 && s.StartMin == s.EndMin
}

// Active reports whether now falls inside the schedule. Callers pass a local
// time so the window is evaluated on the operator's configured clock.
func (s LimitSchedule) Active(now time.Time) bool {
	if s.Empty() {
		return true
	}
	weekday := int(now.Weekday())
	minute := now.Hour()*60 + now.Minute()
	dayAllowed := func(day int) bool {
		if len(s.Days) == 0 {
			return true
		}
		for _, candidate := range s.Days {
			if candidate == day {
				return true
			}
		}
		return false
	}
	if s.StartMin < s.EndMin {
		return dayAllowed(weekday) && minute >= s.StartMin && minute < s.EndMin
	}
	if s.StartMin > s.EndMin {
		if minute >= s.StartMin {
			return dayAllowed(weekday)
		}
		if minute < s.EndMin {
			return dayAllowed((weekday + 6) % 7)
		}
		return false
	}
	return dayAllowed(weekday)
}

// ValidateLimitSchedule rejects out-of-band schedules before they reach the
// scheduler. Normalization would silently clamp them, and deny-by-default is
// the operator API's contract.
func ValidateLimitSchedule(schedule LimitSchedule) error {
	if len(schedule.Days) > 7 {
		return errLimitScheduleDays
	}
	seen := make(map[int]struct{}, len(schedule.Days))
	for _, day := range schedule.Days {
		if day < 0 || day > 6 {
			return errLimitScheduleDays
		}
		if _, ok := seen[day]; ok {
			return errLimitScheduleDays
		}
		seen[day] = struct{}{}
	}
	if schedule.StartMin < 0 || schedule.StartMin >= minutesPerDay ||
		schedule.EndMin < 0 || schedule.EndMin >= minutesPerDay {
		return errLimitScheduleMinutes
	}
	return nil
}

type limitError string

func (e limitError) Error() string { return string(e) }

const (
	errLimitScheduleDays    = limitError("schedule days must be unique weekday numbers 0..6")
	errLimitScheduleMinutes = limitError("schedule minutes must be 0..1439")
	// ErrLimitScopeBound reports a dimension with more values than one policy
	// may carry.
	ErrLimitScopeBound = limitError("scope dimension has too many values")
	// ErrLimitScopeKeyHash reports a key scope value that is not a SHA-256 hex
	// digest. Keys are never accepted or echoed raw.
	ErrLimitScopeKeyHash = limitError("key scope must be a sha-256 hex digest")
)

// ValidateLimitScope enforces the API bounds on one scope. It does not require
// a non-empty scope: a policy with no selector is a global rule.
func ValidateLimitScope(scope LimitScope) error {
	groups := [][]string{scope.Providers, scope.Models, scope.Clients, scope.KeyHashes, scope.Conversations}
	for _, group := range groups {
		if len(group) > maxScopeValues {
			return ErrLimitScopeBound
		}
	}
	for _, key := range scope.KeyHashes {
		if !isSHA256Hex(key) {
			return ErrLimitScopeKeyHash
		}
	}
	return nil
}

func isSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
