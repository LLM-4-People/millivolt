// Package plugincred carries request-scoped provider credentials across the
// proxy/plugin boundary without persisting them in records, logs, descriptors,
// or responses.
package plugincred

import "context"

type requestCredentialKey struct{}

// Credential is the provider auth header and value used by a tracked inference
// request. The value already includes any configured auth prefix.
type Credential struct {
	Header string
	Value  string
}

// With attaches a request-scoped credential to a context for the duration of a
// plugin call.
func With(ctx context.Context, header, value string) context.Context {
	if ctx == nil || header == "" || value == "" {
		return ctx
	}
	return context.WithValue(ctx, requestCredentialKey{}, &Credential{Header: header, Value: value})
}

// From returns the request-scoped credential carried by ctx, if any.
func From(ctx context.Context) *Credential {
	if ctx == nil {
		return nil
	}
	cred, _ := ctx.Value(requestCredentialKey{}).(*Credential)
	return cred
}
