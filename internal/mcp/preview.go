package mcp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// previewTokenTTL is how long a preview authorizes a deletion. It is an
// implementation constant of this server, not a user-tunable: long enough to
// span a model's read-the-count-then-confirm turn, short enough that a token
// pasted into an unrelated conversation an hour later cannot authorize anything.
//
// The lifetime cannot be infinite either way: a token with no expiry would be a
// permanent deletion capability, which is exactly what this guard exists to
// remove.
const previewTokenTTL = 15 * time.Minute

// previewTokenDomain separates this token's MAC from any other HMAC keyed by the
// operator credential, so a signature produced for some other purpose can never
// be replayed here.
const previewTokenDomain = "millivolt-mcp/purge-preview/v1\x00"

// previewToken is the one owner of the token that binds a purge to a real
// preview of the SAME filter.
//
// Why a token and not a count. A count alone cannot bind a deletion to a
// preview: it is only a number, so two different filters that happen to match
// the same number of rows authorize each other, and a count obtained from any
// other source - a query, a guess, a preview of a different filter - satisfies
// the comparison just as well. That is not a theoretical gap: it was used to
// delete a filter nobody had previewed.
//
// The token is stateless on purpose. There is no server-side table of pending
// previews, so nothing has to be persisted, expired or cleaned up, and a token
// stays verifiable for its lifetime, previewTokenTTL (15 minutes), not for the
// whole of a client session: verification refuses it once that window has
// passed. It carries everything
// verification needs - the canonicalized filter, the count and an expiry - and
// is authenticated with an HMAC keyed by the operator credential, so only a
// holder of that credential can mint one.
type previewToken struct {
	key []byte
}

// canonicalFilter is the exact byte string a filter is bound by. It comes from
// the same document() the outgoing request is built from and is encoded by
// encoding/json, which sorts map keys, so two equal filters always produce the
// same bytes and two different filters cannot.
func canonicalFilter(filter PurgeFilterInput) ([]byte, error) {
	return json.Marshal(filter.document())
}

// previewClaims is what a verified token carries. The filter stays as its
// canonical bytes: verification compares them, so nothing ever has to be
// decoded back into a struct and a round trip cannot drift.
type previewClaims struct {
	filter  []byte
	count   int64
	expires time.Time
}

// encode renders the claims. Each part is length-prefixed, so a filter value
// containing a newline can never be read as a different field.
func (c previewClaims) encode() []byte {
	var b strings.Builder
	write := func(part string) {
		b.WriteString(strconv.Itoa(len(part)))
		b.WriteByte(':')
		b.WriteString(part)
		b.WriteByte('\n')
	}
	write(string(c.filter))
	write(strconv.FormatInt(c.count, 10))
	write(strconv.FormatInt(c.expires.Unix(), 10))
	return []byte(b.String())
}

// decodeClaims is the exact inverse of encode, and rejects anything it does not
// fully understand rather than guessing: a partially decoded authorization is
// worse than none.
func decodeClaims(payload []byte) (previewClaims, error) {
	parts := []string{}
	rest := string(payload)
	for len(rest) > 0 {
		colon := strings.IndexByte(rest, ':')
		if colon <= 0 {
			return previewClaims{}, errors.New("malformed preview token")
		}
		length, err := strconv.Atoi(rest[:colon])
		if err != nil || length < 0 || colon+1+length > len(rest) {
			return previewClaims{}, errors.New("malformed preview token")
		}
		parts = append(parts, rest[colon+1:colon+1+length])
		rest = rest[colon+1+length:]
		if len(rest) > 0 {
			if rest[0] != '\n' {
				return previewClaims{}, errors.New("malformed preview token")
			}
			rest = rest[1:]
		}
	}
	if len(parts) != 3 {
		return previewClaims{}, errors.New("malformed preview token")
	}
	count, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return previewClaims{}, errors.New("malformed preview token")
	}
	expiry, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return previewClaims{}, errors.New("malformed preview token")
	}
	return previewClaims{filter: []byte(parts[0]), count: count, expires: time.Unix(expiry, 0).UTC()}, nil
}

// issue mints the token for a real preview of filter at count.
func (t previewToken) issue(filter PurgeFilterInput, count int64, now time.Time) (string, error) {
	canonical, err := canonicalFilter(filter)
	if err != nil {
		return "", err
	}
	claims := previewClaims{filter: canonical, count: count, expires: now.Add(previewTokenTTL).UTC().Truncate(time.Second)}
	payload := claims.encode()
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(t.sign(payload)), nil
}

// errPreviewRefused is the single refusal a bad token produces. A wrong token, a
// never-issued token and a forged one are indistinguishable to the caller on
// purpose: the guard must not become an oracle for what went wrong. An expired
// token and a token for another filter get their own messages because both are
// correctable by running the preview again with the right filter.
var errPreviewRefused = errors.New("preview_token is not a valid authorization for this deletion: it must be the preview_token " +
	"purge_preview returned for THIS exact filter. Run purge_preview again and use the token it returns now")

// verify checks a presented token against the filter the caller is about to
// delete, and returns the count it was authorized at.
func (t previewToken) verify(token string, filter PurgeFilterInput, now time.Time) (int64, error) {
	payload, signature, ok := strings.Cut(token, ".")
	if !ok || payload == "" || signature == "" {
		return 0, errPreviewRefused
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return 0, errPreviewRefused
	}
	presented, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(presented, t.sign(raw)) {
		return 0, errPreviewRefused
	}
	claims, err := decodeClaims(raw)
	if err != nil {
		return 0, errPreviewRefused
	}
	if !now.Before(claims.expires) {
		return 0, errors.New("preview_token has expired: a preview authorizes a deletion only shortly after it was taken. " +
			"Run purge_preview again and use the token it returns now")
	}
	// The binding that matters: this token's filter must BE the filter about to
	// be deleted, byte for byte.
	requested, err := canonicalFilter(filter)
	if err != nil {
		return 0, errPreviewRefused
	}
	if !hmac.Equal(claims.filter, requested) {
		return 0, errors.New("preview_token was issued for a DIFFERENT filter than the one being deleted. " +
			"Run purge_preview with this exact filter and use the token it returns")
	}
	return claims.count, nil
}

// sign is the one MAC. The domain is prepended so a signature over some other
// HMAC payload keyed by the same credential cannot be replayed here.
func (t previewToken) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, t.key)
	mac.Write([]byte(previewTokenDomain))
	mac.Write(payload)
	return mac.Sum(nil)
}

// newPreviewToken derives the key from the operator credential. The credential
// is the one secret this process already holds and it is the right key: a token
// must not outlive the credential that could mint it, so a rotated credential
// invalidates every outstanding preview automatically.
func newPreviewToken(token string) previewToken {
	sum := sha256.Sum256([]byte(previewTokenDomain + token))
	return previewToken{key: sum[:]}
}
