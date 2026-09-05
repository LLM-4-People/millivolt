package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestImageIdentityBoundary(t *testing.T) {
	for _, value := range []string{"millivolt:main", "--privileged", "sha256:abc", "sha256:" + strings.Repeat("G", 64)} {
		if imageID.MatchString(value) {
			t.Fatalf("accepted nonlocal image identity %q", value)
		}
	}
	if !imageID.MatchString("sha256:" + strings.Repeat("a", 64)) {
		t.Fatal("rejected image ID")
	}
}
func TestProbeRejectsHostBeforeHTTP(t *testing.T) {
	t.Setenv(smokeEnv, "")
	if err := probe("seed"); err == nil {
		t.Fatal("host probe accepted")
	}
	if err := probe("unknown"); err == nil {
		t.Fatal("unknown phase accepted")
	}
}
func TestDockerGoVersionMatchesModule(t *testing.T) {
	root := filepath.Join("..", "..")
	dockerfile, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	tag := regexp.MustCompile(`(?m)^FROM --platform=\$BUILDPLATFORM golang:([0-9]+\.[0-9]+\.[0-9]+)-[^@]+@sha256:[a-f0-9]{64} AS build$`).FindSubmatch(dockerfile)
	version := regexp.MustCompile(`(?m)^go ([0-9]+\.[0-9]+\.[0-9]+)$`).FindSubmatch(gomod)
	if len(tag) != 2 || len(version) != 2 || string(tag[1]) != string(version[1]) {
		t.Fatal("Docker Go toolchain must match go.mod and retain its digest pin")
	}
	if !strings.Contains(string(dockerfile), "$(go list -m).revisionOverride=") {
		t.Fatal("linker namespace must follow the module owner")
	}
	if !strings.Contains(string(dockerfile), `-ldflags="-s `) {
		t.Fatal("runtime binary must omit optional symbol/debug tables")
	}
}

func TestDockerContextPolicy(t *testing.T) {
	root := filepath.Join("..", "..")
	data, err := os.ReadFile(filepath.Join(root, ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	rules := []string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			rules = append(rules, line)
		}
	}
	if len(rules) == 0 || rules[0] != "**" {
		t.Fatal("Docker context must deny by default")
	}
	allowed := map[string]bool{}
	// Source roots are deliberately explicit: new dependencies need review before
	// their entire subtree can be transmitted to a local or remote builder.
	for _, path := range []string{"Dockerfile", ".dockerignore", "go.mod", "go.sum", "VERSION", "version.go", "LICENSE", "THIRD_PARTY_NOTICES.md", "cmd/", "cmd/proxy/", "cmd/proxy/**", "internal/", "internal/**", "scripts/", "scripts/licenses/", "scripts/licenses/**"} {
		allowed["!"+path] = true
	}
	finalExclusions := map[string]bool{}
	excluding := false
	for _, rule := range rules[1:] {
		if strings.HasPrefix(rule, "!") {
			if excluding || !allowed[rule] {
				t.Fatalf("unreviewed or late source inclusion %q", rule)
			}
		} else {
			excluding = true
			finalExclusions[rule] = true
		}
	}
	// Git's own metadata is implicitly hidden from publication, but Docker must
	// exclude it explicitly even beneath an allowed source tree.
	if !finalExclusions["**/.git"] {
		t.Error("Docker context must exclude nested Git metadata")
	}
	gitignore, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	// The existing repository ignore policy owns sensitive suffixes. Mirror all
	// unanchored exclusions, including nested private files in allowed source.
	for line := range strings.SplitSeq(string(gitignore), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.ContainsAny(line[:1], "#!/") {
			continue
		}
		if !finalExclusions["**/"+line] {
			t.Errorf("Docker context lacks nested exclusion for %q", line)
		}
	}
}

func TestLocalDaemonPinsEffectiveContext(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "docker")
	const fixture = `#!/bin/sh
if [ "$1 $2" = "context inspect" ]; then
  printf '%s\n' "$FIXTURE_ENDPOINT"
else
  [ "$1" = --host ] && [ "$2" = unix:///fixture.sock ] || exit 20
  [ -z "$DOCKER_HOST$DOCKER_CONTEXT$DOCKER_TLS_VERIFY" ] || exit 21
  printf 'pinned\n'
fi
`
	if err := os.WriteFile(tool, []byte(fixture), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_HOST", "unix:///fixture.sock")
	t.Setenv("DOCKER_CONTEXT", "selected")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("FIXTURE_ENDPOINT", "ssh://remote.example")
	if _, err := localDaemon(context.Background()); err == nil {
		t.Fatal("DOCKER_HOST hid a remote DOCKER_CONTEXT")
	}
	t.Setenv("FIXTURE_ENDPOINT", "unix:///fixture.sock")
	d, err := localDaemon(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONTEXT", "changed-after-verification")
	got, err := d.command(context.Background(), "image", "inspect")
	if err != nil || string(got) != "pinned" {
		t.Fatalf("endpoint was not pinned: %s %v", got, err)
	}
	t.Setenv("DOCKER_CONTEXT", "")
	for _, bad := range []string{"unix://remote/socket", "unix:relative", "unix:///socket?x=y", "tcp://127.0.0.1:2375"} {
		t.Setenv("DOCKER_HOST", bad)
		if _, err := localDaemon(context.Background()); err == nil {
			t.Errorf("accepted endpoint %q", bad)
		}
	}
}

func TestComposeIsolationContract(t *testing.T) {
	const good = `{"services":{"millivolt":{"read_only":true,"ports":[{"host_ip":"127.0.0.1"}],"volumes":[{"type":"volume","source":"data","target":"/data"},{"type":"volume","source":"config","target":"/config"}]}}}`
	if err := validateCompose([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(good, `"read_only":true`, `"read_only":false`, 1),
		strings.Replace(good, "127.0.0.1", "0.0.0.0", 1),
		strings.Replace(good, `"type":"volume"`, `"type":"bind"`, 1),
		strings.Replace(good, `"target":"/config"`, `"target":"/data"`, 1),
		`{"services":{}}`, `null`, `invalid`,
	} {
		if err := validateCompose([]byte(bad)); err == nil {
			t.Errorf("accepted unsafe Compose model %s", bad)
		}
	}
}
