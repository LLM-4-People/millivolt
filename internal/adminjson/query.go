package adminjson

import (
	"fmt"
	"net/http"
	"net/url"
)

// StrictQuery is the one strict query-string parse every query-reading route
// handler shares. It parses the raw string itself, so a malformed pair - an
// invalid percent escape, a semicolon separator - is an error instead of
// being silently dropped the way http.Request.URL.Query's lenient read drops
// it (a dropped flag on a destructive plane silently broadens the action; a
// dropped filter silently broadens a read). Empty components stay the
// stdlib's silently-skipped convention, never an error. On error no
// half-parsed values escape: callers must discard the values whenever the
// error is non-nil. Well-formed query strings parse byte-identically to the
// lenient read. StrictQuery does not write the response; each caller answers
// the error through its own pinned error transport.
func StrictQuery(r *http.Request) (url.Values, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	return q, nil
}

// DuplicateQueryKey enforces the repeated-key rule every query-reading route
// applies to the keys it consumes, in the export owner's wording: a repeated
// occurrence is ambiguous, deny it rather than guess which occurrence the
// client meant. keys lists the route's consumed single-occurrence keys in
// check order; a legitimately repeated list key (the explorer's f=) simply
// stays off the list. It returns nil when every listed key occurs at most
// once. DuplicateQueryKey does not write the response; each caller answers
// the error through its own pinned error transport.
func DuplicateQueryKey(q url.Values, keys ...string) error {
	for _, key := range keys {
		if len(q[key]) > 1 {
			return fmt.Errorf("duplicate %s", key)
		}
	}
	return nil
}

// UnknownQueryKey enforces the closed key set of the mutating routes: a
// query key outside the consumed list is an error, never silently ignored,
// because on a mutating route an ignored key selects the default, broader
// action (?INSPECT=1 would run the real restore where ?inspect=1 previews).
// The idempotent fetches keep the documented ignore class instead. keys
// lists every key the route consumes; an unlisted key names itself in the
// export owner's wording class ("unknown key <name>"). With several unknown
// keys present the reported one follows map iteration order - the status is
// the contract there, exactly like the export owner's multi-violation
// answer. UnknownQueryKey does not write the response; each caller answers
// the error through its own pinned error transport.
func UnknownQueryKey(q url.Values, keys ...string) error {
	consumed := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		consumed[key] = struct{}{}
	}
	for key := range q {
		if _, ok := consumed[key]; !ok {
			return fmt.Errorf("unknown key %s", key)
		}
	}
	return nil
}
