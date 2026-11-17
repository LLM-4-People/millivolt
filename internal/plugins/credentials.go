package plugins

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

// CredentialsMaxBytes and CredentialsMaxEntries bound the credential file
// before it is read or decoded. The file stores SHA-256 digests and scopes,
// never raw tokens, so a settings backup cannot export a usable credential.
const (
	CredentialsMaxBytes   = 64 << 10
	CredentialsMaxEntries = 64
)

type credentialFile struct {
	Credentials []credentialEntry `json:"credentials"`
}

type credentialEntry struct {
	ID     string   `json:"id"`
	Hash   string   `json:"hash"`
	Scopes []string `json:"scopes"`
}

type credential struct {
	id     string
	hash   [sha256.Size]byte
	scopes mcp.CapabilitySet
}

// Credentials is an immutable set of scoped plugin tokens loaded at boot.
type Credentials struct {
	entries []credential
}

// LoadCredentials reads one bounded JSON document. An empty path disables
// scoped credentials entirely. Any malformed entry fails the whole load: there
// is no partial allowlist that could grant more than the operator intended.
func LoadCredentials(path string) (*Credentials, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("plugin credentials %q: %w", path, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, CredentialsMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("plugin credentials %q: %w", path, err)
	}
	if len(raw) > CredentialsMaxBytes {
		return nil, fmt.Errorf("plugin credentials %q: file exceeds the %d byte cap", path, CredentialsMaxBytes)
	}
	var doc credentialFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("plugin credentials %q: %w", path, err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("plugin credentials %q: trailing JSON content", path)
	}
	if doc.Credentials == nil || len(doc.Credentials) == 0 {
		return nil, fmt.Errorf("plugin credentials %q: no credentials", path)
	}
	if len(doc.Credentials) > CredentialsMaxEntries {
		return nil, fmt.Errorf("plugin credentials %q: more than %d credentials", path, CredentialsMaxEntries)
	}
	store := &Credentials{entries: make([]credential, 0, len(doc.Credentials))}
	seenID := make(map[string]bool, len(doc.Credentials))
	seenHash := make(map[[sha256.Size]byte]bool, len(doc.Credentials))
	for i, entry := range doc.Credentials {
		if entry.ID == "" || len(entry.ID) > 128 || strings.ContainsAny(entry.ID, " \t\r\n") {
			return nil, fmt.Errorf("plugin credentials[%d]: invalid id", i)
		}
		if seenID[entry.ID] {
			return nil, fmt.Errorf("plugin credentials[%d]: duplicate id %q", i, entry.ID)
		}
		seenID[entry.ID] = true
		decoded, err := hex.DecodeString(entry.Hash)
		if err != nil || len(decoded) != sha256.Size || strings.ToLower(entry.Hash) != entry.Hash {
			return nil, fmt.Errorf("plugin credentials[%d]: hash must be %d lowercase hex characters", i, sha256.Size*2)
		}
		var hash [sha256.Size]byte
		copy(hash[:], decoded)
		if seenHash[hash] {
			return nil, fmt.Errorf("plugin credentials[%d]: duplicate hash", i)
		}
		seenHash[hash] = true
		if len(entry.Scopes) == 0 {
			return nil, fmt.Errorf("plugin credentials[%d]: at least one scope is required", i)
		}
		scopes, err := mcp.ParseCapabilities(entry.Scopes)
		if err != nil {
			return nil, fmt.Errorf("plugin credentials[%d]: %w", i, err)
		}
		store.entries = append(store.entries, credential{id: entry.ID, hash: hash, scopes: scopes})
	}
	return store, nil
}

// Lookup returns the capability set for a presented token. The comparison is
// over digests and constant-time for every stored entry, so neither token
// content nor length leaks through the credential store.
func (c *Credentials) Lookup(token string) (mcp.CapabilitySet, bool) {
	if c == nil || token == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte(token))
	for _, entry := range c.entries {
		if subtle.ConstantTimeCompare(sum[:], entry.hash[:]) == 1 {
			return entry.scopes, true
		}
	}
	return nil, false
}

// Len is the number of loaded scoped credentials.
func (c *Credentials) Len() int {
	if c == nil {
		return 0
	}
	return len(c.entries)
}
