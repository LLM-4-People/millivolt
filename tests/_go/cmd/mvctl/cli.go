package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestHelpListsEveryCommand(t *testing.T) {
	code, stdout, stderr := runCLI(t, "help")
	if code != 0 || stderr != "" {
		t.Fatalf("help = %d, stderr %q", code, stderr)
	}
	for _, cmd := range registry() {
		if !strings.Contains(stdout, cmd.Name) {
			t.Errorf("help does not list %q:\n%s", cmd.Name, stdout)
		}
		if cmd.Summary == "" {
			t.Errorf("command %q has no summary", cmd.Name)
		}
	}
}

func TestCommandHelpAndVersion(t *testing.T) {
	code, stdout, _ := runCLI(t, "help", "requests")
	if code != 0 || !strings.Contains(stdout, "--before-ms") {
		t.Fatalf("command help = %d, %q", code, stdout)
	}
	code, stdout, _ = runCLI(t, "--version")
	if code != 0 || !strings.HasPrefix(stdout, "mvctl ") {
		t.Fatalf("version = %d, %q", code, stdout)
	}
}

func TestUnknownFlagsAndCommands(t *testing.T) {
	for _, args := range [][]string{
		{"status", "--bogus"},
		{"--bogus", "status"},
		{"bogus"},
		{"requests", "--limit", "5"},
		{"--", "bogus"},
		{"--help=false", "status", "--bogus"},
	} {
		code, _, stderr := runCLI(t, args...)
		if code != 2 {
			t.Errorf("%v = %d, want usage exit 2 (stderr %q)", args, code, stderr)
		}
	}
}

func TestBooleanAndDoubleDash(t *testing.T) {
	code, stdout, _ := runCLI(t, "--url", "http://127.0.0.1:8080/", "url")
	if code != 0 || strings.TrimSpace(stdout) != "http://127.0.0.1:8080" {
		t.Fatalf("url = %d, %q", code, stdout)
	}
	code, stdout, _ = runCLI(t, "--url", "http://127.0.0.1:8080", "--json=false", "url")
	if code != 0 || strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Fatalf("--json=false = %d, %q", code, stdout)
	}
	inv, err := parseInvocation([]string{"query", "--", "--json"}, registry())
	if err != nil || inv.command == nil || inv.command.Name != "query" ||
		len(inv.args) != 1 || inv.args[0] != "--json" || inv.global.JSON {
		t.Fatalf("double dash = %+v, %v", inv, err)
	}
	// --help=false must execute the command, not print help.
	code, stdout, _ = runCLI(t, "--url", "http://127.0.0.1:8080", "--help=false", "url")
	if code != 0 || strings.TrimSpace(stdout) != "http://127.0.0.1:8080" {
		t.Fatalf("--help=false = %d, %q", code, stdout)
	}
	// -- ends flag parsing; the next word may name the command.
	code, stdout, _ = runCLI(t, "--url", "http://127.0.0.1:8080", "--", "url")
	if code != 0 || strings.TrimSpace(stdout) != "http://127.0.0.1:8080" {
		t.Fatalf("double dash command = %d, %q", code, stdout)
	}
}

func TestURLRequiresAnExplicitOrigin(t *testing.T) {
	t.Setenv(proxyURLEnv, "")
	code, _, stderr := runCLI(t, "url")
	if code != 2 || !strings.Contains(stderr, "proxy URL is required") {
		t.Fatalf("url without an origin = %d, %q", code, stderr)
	}
}

func TestURLRejectsBadOrigin(t *testing.T) {
	for _, raw := range []string{
		"ftp://127.0.0.1:8080",
		"http://user:pass@127.0.0.1:8080",
		"http://127.0.0.1:8080/admin",
		"http://127.0.0.1:8080/?q=1",
	} {
		code, _, stderr := runCLI(t, "--url", raw, "url")
		if code != 2 {
			t.Errorf("url %q = %d, want 2 (%q)", raw, code, stderr)
		}
	}
}

func TestParseBool(t *testing.T) {
	for _, raw := range []string{"true", "1", "yes", "on", ""} {
		if value, err := parseBool(raw); err != nil || !value {
			t.Errorf("parseBool(%q) = %v, %v", raw, value, err)
		}
	}
	for _, raw := range []string{"false", "0", "no", "off"} {
		if value, err := parseBool(raw); err != nil || value {
			t.Errorf("parseBool(%q) = %v, %v", raw, value, err)
		}
	}
	if _, err := parseBool("maybe"); err == nil {
		t.Fatal("parseBool accepted an ambiguous spelling")
	}
}

func TestDocsListEveryCommand(t *testing.T) {
	raw, err := os.ReadFile("../../docs/cli.md")
	if err != nil {
		t.Fatalf("read docs/cli.md: %v", err)
	}
	doc := string(raw)
	for _, cmd := range registry() {
		want := "`mvctl " + cmd.Name
		if !strings.Contains(doc, want) {
			t.Errorf("docs/cli.md does not document %q", want)
		}
	}
	for _, spec := range globalFlagSpecs(defaultSettings()) {
		if !strings.Contains(doc, "--"+spec.name) {
			t.Errorf("docs/cli.md does not document global flag --%s", spec.name)
		}
	}
	for _, cmd := range registry() {
		for _, spec := range cmd.Flags {
			if !strings.Contains(doc, "--"+spec.name) {
				t.Errorf("docs/cli.md does not document --%s for %s", spec.name, cmd.Name)
			}
		}
	}
}

func TestTableKeepsSemanticColorAndStripsUntrustedEscapes(t *testing.T) {
	setColor(true)
	defer setColor(false)
	tableOut := table([]string{"status", "label"}, [][]string{{statusCode(200), "evil\x1b[31mred"}})
	if strings.Contains(tableOut, "\x1b") {
		t.Fatalf("table emitted a raw escape before render: %q", tableOut)
	}
	rendered := render(tableOut)
	if !strings.Contains(rendered, "\x1b[32m200") {
		t.Fatalf("semantic status color was stripped from a table cell: %q", rendered)
	}
	if strings.Contains(rendered, "\x1b[31mred") {
		t.Fatalf("an untrusted escape survived sanitization: %q", rendered)
	}
	if strings.Contains(strip(tableOut), string(styleStart)) {
		t.Fatalf("width accounting left a style sentinel visible: %q", tableOut)
	}
	foreign := string(styleStart) + "31mEVIL" + string(styleEnd)
	sanitized := render(sanitizeCell(foreign))
	if strings.ContainsRune(sanitized, styleStart) || strings.ContainsRune(sanitized, styleEnd) || strings.Contains(sanitized, "\x1b") {
		t.Fatalf("a foreign sentinel was rendered as an escape: %q", sanitized)
	}
	if got := width(green("200")); got != 3 {
		t.Fatalf("width of a styled cell = %d, want 3", got)
	}
	if got := width("a" + green("b") + "c"); got != 3 {
		t.Fatalf("width of mixed styled text = %d, want 3", got)
	}
}

func TestShortFlagsComeFromTheRegistry(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/healthz": jsonHandler(t, `{"ok":true}`),
	})
	code, stdout, _ := runCLI(t, "--url", fixture.url(), "-j", "health")
	if code != 0 || !strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Fatalf("-j = %d, %q", code, stdout)
	}
	code, stdout, _ = runCLI(t, "-h")
	if code != 0 || !strings.Contains(stdout, "Usage: mvctl") {
		t.Fatalf("-h = %d, %q", code, stdout)
	}
}

func TestSubcommandsRejectFlagsTheyDoNotOwn(t *testing.T) {
	fixture := newProxy(t, map[string]http.HandlerFunc{
		"/admin/pause":    func(w http.ResponseWriter, _ *http.Request) { t.Error("pause received a flag it does not own") },
		"/admin/quota":    func(w http.ResponseWriter, _ *http.Request) { t.Error("quota received a flag it does not own") },
		"/admin/debug":    func(w http.ResponseWriter, _ *http.Request) { t.Error("debug received a flag it does not own") },
		"/admin/throttle": func(w http.ResponseWriter, _ *http.Request) { t.Error("throttle received a flag it does not own") },
		"/admin/config":   func(w http.ResponseWriter, _ *http.Request) { t.Error("config received a flag it does not own") },
	})
	for _, args := range [][]string{
		{"pause", "show", "--max-queued", "1"},
		{"quota", "show", "--provider", "p"},
		{"debug", "show", "--id", "d"},
		{"throttle", "show", "--provider", "p"},
		{"config", "keys", "--reveal"},
		{"config", "reload", "--reveal"},
	} {
		code, _, stderr := runCLI(t, append([]string{"--url", fixture.url()}, args...)...)
		if code != 2 || !strings.Contains(stderr, "not valid for this subcommand") {
			t.Errorf("%v = %d, stderr %q, want a subcommand flag refusal", args, code, stderr)
		}
	}
}

func TestDocsMatchExitCodesAndSettingsDefaults(t *testing.T) {
	raw, err := os.ReadFile("../../docs/cli.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, code := range []int{exitOK, exitFailure, exitUsageCode, exitInterrupted} {
		if !strings.Contains(doc, fmt.Sprintf("| %d |", code)) {
			t.Errorf("docs/cli.md does not document exit code %d", code)
		}
	}
	settings := defaultSettings()
	if !strings.Contains(doc, fmt.Sprintf("default to %t", settings.BackupConfig)) {
		t.Errorf("docs/cli.md does not state the backup membership default %t", settings.BackupConfig)
	}
	if !strings.Contains(doc, "default mode is `"+settings.RestoreConfigMode+"`") {
		t.Errorf("docs/cli.md does not state the restore mode default %q", settings.RestoreConfigMode)
	}
}
