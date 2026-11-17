package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	sourceTimeout        = 30 * time.Second
	sourceMaxRedirects   = 5
	sourceMaxReleaseJSON = 256 << 10
)

var (
	allowedSourceHosts = map[string]bool{
		"api.github.com":                       true,
		"github.com":                           true,
		"objects.githubusercontent.com":        true,
		"release-assets.githubusercontent.com": true,
	}
	sourceAssetCDNHosts = map[string]bool{
		"objects.githubusercontent.com":        true,
		"release-assets.githubusercontent.com": true,
	}
	githubNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
	pluginAssetPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}\.mvplugin$`)
)

// FetchedPlugin is one validated plugin archive from a remote source.
type FetchedPlugin struct {
	Raw             []byte
	Package         *Package
	Source          string
	ResolvedVersion string
	AssetURL        string
}

// SourceFetcher resolves a source reference into one bounded plugin archive.
type SourceFetcher interface {
	Fetch(context.Context, string) (FetchedPlugin, error)
}

// GitHubSource resolves signed plugin releases from GitHub. It uses plain
// bounded HTTP and only allowlisted HTTPS hosts.
type GitHubSource struct {
	client *http.Client
}

// NewGitHubSource builds the production GitHub source client.
func NewGitHubSource() *GitHubSource {
	return NewGitHubSourceWithTransport(nil)
}

// NewGitHubSourceWithTransport accepts an injected transport for tests. The
// redirect and host policy stays on this type regardless of transport.
func NewGitHubSourceWithTransport(transport http.RoundTripper) *GitHubSource {
	client := &http.Client{
		Timeout:       sourceTimeout,
		CheckRedirect: checkSourceRedirect,
	}
	if transport != nil {
		client.Transport = transport
	}
	return &GitHubSource{client: client}
}

// Fetch resolves ref, downloads the selected .mvplugin asset and validates its
// archive shape. Signature and digest-pin policy remain with the installer.
func (s *GitHubSource) Fetch(ctx context.Context, ref string) (FetchedPlugin, error) {
	if s == nil || s.client == nil {
		return FetchedPlugin{}, fmt.Errorf("plugin source client is not configured")
	}
	parsed, err := parseGitHubRef(ref)
	if err != nil {
		return FetchedPlugin{}, err
	}
	assetURL := parsed.assetURL
	tag := parsed.tag
	if assetURL == "" {
		release, err := s.fetchRelease(ctx, parsed.owner, parsed.repo, parsed.tag)
		if err != nil {
			return FetchedPlugin{}, err
		}
		tag = release.Tag
		assetURL = release.AssetURL
	}
	raw, err := s.fetchBytes(ctx, assetURL, maxPackageArchiveBytes)
	if err != nil {
		return FetchedPlugin{}, err
	}
	pkg, err := ReadPackage(raw)
	if err != nil {
		return FetchedPlugin{}, err
	}
	source := "github:" + parsed.owner + "/" + parsed.repo + "@" + tag
	return FetchedPlugin{
		Raw:             raw,
		Package:         pkg,
		Source:          source,
		ResolvedVersion: tag,
		AssetURL:        assetURL,
	}, nil
}

type githubRef struct {
	owner    string
	repo     string
	tag      string
	assetURL string
}

type githubRelease struct {
	Tag      string
	AssetURL string
}

type releaseDocument struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		State              string `json:"state"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func parseGitHubRef(raw string) (githubRef, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return githubRef{}, fmt.Errorf("invalid plugin source")
	}
	if strings.HasPrefix(raw, "github:") {
		return parseGitHubShort(raw)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return githubRef{}, fmt.Errorf("invalid plugin source URL")
	}
	if parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return githubRef{}, fmt.Errorf("plugin source URL must be a clean HTTPS URL")
	}
	if parsed.Host != "github.com" {
		return githubRef{}, fmt.Errorf("plugin source host is not allowed")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 6 || parts[2] != "releases" || parts[3] != "download" {
		return githubRef{}, fmt.Errorf("plugin source must be a GitHub release asset URL")
	}
	owner, repo, tag, asset := parts[0], parts[1], parts[4], parts[5]
	if err := validateGitHubIdentity(owner, repo); err != nil {
		return githubRef{}, err
	}
	if !pluginSemverPattern.MatchString(tag) || !pluginAssetPattern.MatchString(asset) {
		return githubRef{}, fmt.Errorf("plugin release tag or asset is invalid")
	}
	return githubRef{owner: owner, repo: repo, tag: tag, assetURL: raw}, nil
}

func parseGitHubShort(raw string) (githubRef, error) {
	rest := strings.TrimPrefix(raw, "github:")
	at := strings.LastIndexByte(rest, '@')
	if at <= 0 || at == len(rest)-1 {
		return githubRef{}, fmt.Errorf("plugin source must include owner/repo@tag")
	}
	target := rest[at+1:]
	ownerRepo := rest[:at]
	slash := strings.IndexByte(ownerRepo, '/')
	if slash <= 0 || slash == len(ownerRepo)-1 {
		return githubRef{}, fmt.Errorf("plugin source must include owner/repo")
	}
	owner, repo := ownerRepo[:slash], ownerRepo[slash+1:]
	if err := validateGitHubIdentity(owner, repo); err != nil {
		return githubRef{}, err
	}
	if target != "latest" && !pluginSemverPattern.MatchString(target) {
		return githubRef{}, fmt.Errorf("plugin release tag must be a semantic version or latest")
	}
	return githubRef{owner: owner, repo: repo, tag: target}, nil
}

func validateGitHubIdentity(owner, repo string) error {
	if !githubNamePattern.MatchString(owner) || !githubNamePattern.MatchString(repo) {
		return fmt.Errorf("invalid GitHub owner or repository")
	}
	return nil
}

func (s *GitHubSource) fetchRelease(ctx context.Context, owner, repo, tag string) (githubRelease, error) {
	endpoint := "https://api.github.com/repos/" + owner + "/" + repo + "/releases/latest"
	if tag != "latest" {
		endpoint = "https://api.github.com/repos/" + owner + "/" + repo + "/releases/tags/" + url.PathEscape(tag)
	}
	raw, err := s.fetchBytes(ctx, endpoint, sourceMaxReleaseJSON)
	if err != nil {
		return githubRelease{}, err
	}
	var doc releaseDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return githubRelease{}, fmt.Errorf("invalid GitHub release document")
	}
	if !pluginSemverPattern.MatchString(doc.TagName) {
		return githubRelease{}, fmt.Errorf("GitHub release tag is not a semantic version")
	}
	for _, asset := range doc.Assets {
		if asset.State != "" && asset.State != "uploaded" {
			continue
		}
		if !pluginAssetPattern.MatchString(asset.Name) {
			continue
		}
		ref, err := parseGitHubRef(asset.BrowserDownloadURL)
		if err != nil || ref.assetURL == "" {
			continue
		}
		return githubRelease{Tag: doc.TagName, AssetURL: ref.assetURL}, nil
	}
	return githubRelease{}, fmt.Errorf("GitHub release has no .mvplugin asset")
}

func (s *GitHubSource) fetchBytes(ctx context.Context, rawURL string, limit int) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || !allowedSourceHosts[parsed.Hostname()] {
		return nil, fmt.Errorf("plugin source URL is not allowed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "millivolt-plugin-installer")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plugin source returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("plugin source exceeds the %d byte cap", limit)
	}
	return raw, nil
}

func checkSourceRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= sourceMaxRedirects {
		return fmt.Errorf("too many plugin source redirects")
	}
	if req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Fragment != "" {
		return fmt.Errorf("plugin source redirect is not allowed")
	}
	// GitHub signs release-asset redirects with query parameters. Keep queries
	// on the asset CDNs only; API and github.com redirects stay query-free.
	if req.URL.RawQuery != "" && !sourceAssetCDNHosts[req.URL.Hostname()] {
		return fmt.Errorf("plugin source redirect query is not allowed")
	}
	if !allowedSourceHosts[req.URL.Hostname()] {
		return fmt.Errorf("plugin source redirect host is not allowed")
	}
	return nil
}

// latestSource converts a pinned source into a latest-release query.
func latestSource(source string) (string, error) {
	ref, err := parseGitHubRef(source)
	if err != nil {
		return "", err
	}
	return "github:" + ref.owner + "/" + ref.repo + "@latest", nil
}

// InstallSource fetches one signed remote archive and installs it through the
// same bounded core installer used by local uploads.
func InstallSource(ctx context.Context, source, pluginDir, lockPath string, trust *Trust, fetcher SourceFetcher) (InstallReport, error) {
	if trust == nil || !trust.Configured() {
		return InstallReport{}, fmt.Errorf("remote plugin installs require plugin_trust_file")
	}
	if fetcher == nil {
		return InstallReport{}, fmt.Errorf("plugin source fetcher is not configured")
	}
	fetched, err := fetcher.Fetch(ctx, source)
	if err != nil {
		return InstallReport{}, err
	}
	if err := trust.Verify(fetched.Package.Manifest); err != nil {
		return InstallReport{}, err
	}
	tmp, err := stageArchiveFile(os.TempDir(), "remote.mvplugin", fetched.Raw)
	if err != nil {
		return InstallReport{}, err
	}
	defer os.Remove(tmp)
	return installFile(tmp, pluginDir, lockPath, fetched.Source, trust, false)
}
