package plugins

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type sourceRoundTrip func(*http.Request) (*http.Response, error)

func (f sourceRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func sourceResponse(body []byte) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Header:     make(http.Header),
	}
}

func signedArchiveBytes(t *testing.T, id, version string, key ed25519.PrivateKey, signer string, capabilities ...string) []byte {
	t.Helper()
	if len(capabilities) == 0 {
		capabilities = []string{"emit_metric"}
	}
	module := fixtureModule()
	sum := sha256.Sum256(module)
	m := &PluginManifest{
		ID:           id,
		Version:      version,
		ABI:          "v1",
		MinMillivolt: packageTestMinMillivolt,
		Capabilities: append([]string(nil), capabilities...),
		Module:       packageModuleName,
		ModuleSHA256: hex.EncodeToString(sum[:]),
		License:      "MIT",
		Signer:       signer,
	}
	m.Signature = hex.EncodeToString(ed25519.Sign(key, m.SigningPayload()))
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return makePackageZip(t,
		packageZipEntry{name: packageManifestName, data: raw},
		packageZipEntry{name: packageModuleName, data: module},
	)
}

func TestParseGitHubRefRejectsUnsafeSources(t *testing.T) {
	bad := []string{
		"github:owner/repo@main",
		"github:owner/repo",
		"github:../repo@v0.1.0",
		"http://github.com/owner/repo/releases/download/v0.1.0/millivolt-fixture.mvplugin",
		"https://example.com/owner/repo/releases/download/v0.1.0/millivolt-fixture.mvplugin",
		"https://github.com/owner/repo/raw/main/plugin.mvplugin",
		"https://github.com/owner/repo/releases/download/v0.1.0/millivolt-fixture.mvplugin?x=1",
	}
	for _, ref := range bad {
		if _, err := parseGitHubRef(ref); err == nil {
			t.Fatalf("accepted unsafe source %q", ref)
		}
	}
}

func TestParseGitHubRefAcceptsReleaseForms(t *testing.T) {
	short, err := parseGitHubRef("github:owner/repo@v0.1.0")
	if err != nil || short.owner != "owner" || short.repo != "repo" || short.tag != "v0.1.0" {
		t.Fatalf("short = %+v, %v", short, err)
	}
	direct, err := parseGitHubRef("https://github.com/owner/repo/releases/download/v0.1.0/millivolt-fixture.mvplugin")
	if err != nil || direct.assetURL == "" {
		t.Fatalf("direct = %+v, %v", direct, err)
	}
}

func TestGitHubSourceFetch(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	archive := signedArchiveBytes(t, "fixture", "0.1.0", key, "release")
	assetURL := "https://github.com/owner/repo/releases/download/v0.1.0/millivolt-fixture.mvplugin"
	releaseJSON := []byte(`{"tag_name":"v0.1.0","assets":[{"name":"millivolt-fixture.mvplugin","state":"uploaded","browser_download_url":"` + assetURL + `"}]}`)
	calls := 0
	client := NewGitHubSourceWithTransport(sourceRoundTrip(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host == "api.github.com" {
			return sourceResponse(releaseJSON), nil
		}
		if req.URL.String() == assetURL {
			return sourceResponse(archive), nil
		}
		t.Fatalf("unexpected URL %q", req.URL)
		return nil, nil
	}))
	fetched, err := client.Fetch(context.Background(), "github:owner/repo@latest")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || fetched.Source != "github:owner/repo@v0.1.0" || fetched.ResolvedVersion != "v0.1.0" {
		t.Fatalf("fetch = %+v, calls=%d", fetched, calls)
	}
	if string(fetched.Raw) != string(archive) {
		t.Fatal("fetch changed archive bytes")
	}
}

func TestGitHubSourceAllowsSignedAssetRedirectQuery(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	archive := signedArchiveBytes(t, "fixture", "0.1.0", key, "release")
	assetURL := "https://github.com/owner/repo/releases/download/v0.1.0/millivolt-fixture.mvplugin"
	redirectURL := "https://release-assets.githubusercontent.com/asset?sig=abc"
	releaseJSON := []byte(`{"tag_name":"v0.1.0","assets":[{"name":"millivolt-fixture.mvplugin","state":"uploaded","browser_download_url":"` + assetURL + `"}]}`)
	calls := 0
	client := NewGitHubSourceWithTransport(sourceRoundTrip(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host == "api.github.com" {
			return sourceResponse(releaseJSON), nil
		}
		if req.URL.String() == assetURL {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{redirectURL}},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		}
		if req.URL.String() == redirectURL {
			return sourceResponse(archive), nil
		}
		t.Fatalf("unexpected URL %q", req.URL)
		return nil, nil
	}))
	fetched, err := client.Fetch(context.Background(), "github:owner/repo@v0.1.0")
	if err != nil || calls != 3 {
		t.Fatalf("fetch = %+v, calls=%d, err=%v", fetched, calls, err)
	}
	if string(fetched.Raw) != string(archive) {
		t.Fatal("redirect changed archive bytes")
	}
}

func TestCheckUpdatesReportsNewerRelease(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	archive := signedArchiveBytes(t, "fixture", "0.2.0", key, "release")
	assetURL := "https://github.com/owner/repo/releases/download/v0.2.0/millivolt-fixture.mvplugin"
	releaseJSON := []byte(`{"tag_name":"v0.2.0","assets":[{"name":"millivolt-fixture.mvplugin","state":"uploaded","browser_download_url":"` + assetURL + `"}]}`)
	client := NewGitHubSourceWithTransport(sourceRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "api.github.com" {
			return sourceResponse(releaseJSON), nil
		}
		return sourceResponse(archive), nil
	}))
	locks := &Locks{
		Pins: map[string]string{},
		Installs: map[string]InstallState{
			"fixture": {
				Version:       "0.1.0",
				Archive:       "fixture.mvplugin",
				Source:        "github:owner/repo@v0.1.0",
				ArchiveSHA256: strings.Repeat("a", 64),
				InstalledAt:   "2026-01-01T00:00:00Z",
			},
		},
	}
	candidates, err := CheckUpdates(context.Background(), "fixture", locks, client)
	if err != nil || len(candidates) != 1 || candidates[0].Version != "0.2.0" {
		t.Fatalf("candidates = %+v, %v", candidates, err)
	}
}
