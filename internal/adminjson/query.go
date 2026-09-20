package adminjson

import (
	"fmt"
	"net/http"
	"net/url"
)

// StrictQuery is the one strict query-string parse every query-reading route
// handler shares. It parses the raw string itself, so a malformed pair - an
// invalid percent escape, a bare separator - is an error instead of being
// silently dropped the way http.Request.URL.Query's lenient read drops it (a
// dropped flag on a destructive plane silently broadens the action; a dropped
// filter silently broadens a read). On error no half-parsed values escape:
// callers must discard the values whenever the error is non-nil. Well-formed
// query strings parse byte-identically to the lenient read. StrictQuery does
// not write the response; each caller answers the error through its own
// pinned error transport.
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
