package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pluginTestToken = "plugin-scoped-credential-0123456789"

func writeCredentials(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func TestLoadCredentialsAndLookup(t *testing.T) {
	path := writeCredentials(t, fmt.Sprintf(
		`{"credentials":[{"id":"analyzer","hash":%q,"scopes":["read:metrics","read:records"]}]}`,
		tokenHash(pluginTestToken),
	))
	store, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if store.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", store.Len())
	}
	scopes, ok := store.Lookup(pluginTestToken)
	if !ok || !scopes.Allows("read:metrics") || scopes.Allows("admin:config") {
		t.Fatalf("Lookup scopes = %v, ok = %v", scopes.Strings(), ok)
	}
	if _, ok := store.Lookup("wrong-token"); ok {
		t.Fatal("wrong token must not authenticate")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), pluginTestToken) {
		t.Fatal("credential file must contain only a digest, never the token")
	}
}

func TestLoadCredentialsRefusesBadEntries(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"unknown capability", fmt.Sprintf(`{"credentials":[{"id":"a","hash":%q,"scopes":["read:all"]}]}`, tokenHash(pluginTestToken)), "unknown capability"},
		{"bad hash", `{"credentials":[{"id":"a","hash":"xyz","scopes":["read:metrics"]}]}`, "hash must be"},
		{"empty scopes", fmt.Sprintf(`{"credentials":[{"id":"a","hash":%q,"scopes":[]}]}`, tokenHash(pluginTestToken)), "at least one scope"},
		{"duplicate id", fmt.Sprintf(`{"credentials":[{"id":"a","hash":%q,"scopes":["read:metrics"]},{"id":"a","hash":%q,"scopes":["read:metrics"]}]}`, tokenHash(pluginTestToken), tokenHash("other-token-0123456789")), "duplicate id"},
		{"unknown field", fmt.Sprintf(`{"credentials":[{"id":"a","hash":%q,"scopes":["read:metrics"],"note":"x"}]}`, tokenHash(pluginTestToken)), "unknown field"},
		{"trailing JSON", fmt.Sprintf(`{"credentials":[{"id":"a","hash":%q,"scopes":["read:metrics"]}]} {}`, tokenHash(pluginTestToken)), "trailing JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadCredentials(writeCredentials(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadCredentials error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadCredentialsBoundsFile(t *testing.T) {
	path := writeCredentials(t, strings.Repeat("x", CredentialsMaxBytes+1))
	if _, err := LoadCredentials(path); err == nil || !strings.Contains(err.Error(), "byte cap") {
		t.Fatalf("LoadCredentials error = %v, want the byte cap", err)
	}
}

func TestLoadCredentialsEmptyPathDisables(t *testing.T) {
	store, err := LoadCredentials("")
	if err != nil || store != nil {
		t.Fatalf("LoadCredentials(\"\") = %v, %v, want nil, nil", store, err)
	}
	if _, ok := store.Lookup(pluginTestToken); ok {
		t.Fatal("nil store must not authenticate")
	}
}
