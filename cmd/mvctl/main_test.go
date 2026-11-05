package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseGlobalDefaults(t *testing.T) {
	t.Setenv("MILLIVOLT_URL", "")
	t.Setenv("MILLIVOLT_OPERATOR_TOKEN", "")
	p, err := parseGlobal([]string{"status"})
	if err != nil {
		t.Fatal(err)
	}
	if p.g.BaseURL != "http://127.0.0.1:8080" || p.g.Token != "" || !p.g.Color {
		t.Fatalf("defaults: %+v", p.g)
	}
	if len(p.args) != 1 || p.args[0] != "status" {
		t.Fatalf("args: %v", p.args)
	}
}

func TestParseGlobalEnvAndFlags(t *testing.T) {
	t.Setenv("MILLIVOLT_URL", "http://127.0.0.1:8471")
	t.Setenv("MILLIVOLT_OPERATOR_TOKEN", "env-value")
	p, err := parseGlobal([]string{"--json", "--no-color", "--token", "flag-value", "--url=http://127.0.0.1:9", "overview"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.g.JSON || p.g.Color || p.g.Token != "flag-value" || p.g.BaseURL != "http://127.0.0.1:9" {
		t.Fatalf("globals: %+v", p.g)
	}
	if p.args[0] != "overview" {
		t.Fatalf("args: %v", p.args)
	}
}

func TestParseGlobalPerCommandFlags(t *testing.T) {
	t.Setenv("MILLIVOLT_URL", "")
	p, err := parseGlobal([]string{"purge", "--client", "mvctl-demo", "--yes"})
	if err != nil {
		t.Fatal(err)
	}
	if p.flags["client"] != "mvctl-demo" || !p.g.Yes {
		t.Fatalf("flags=%+v global=%+v", p.flags, p.g)
	}
	if len(p.args) != 1 || p.args[0] != "purge" {
		t.Fatalf("args: %v", p.args)
	}
}

func TestParseGlobalTimeout(t *testing.T) {
	t.Setenv("MILLIVOLT_URL", "")
	p, _ := parseGlobal([]string{"--timeout", "3s", "status"})
	if p.g.Timeout != 3*time.Second {
		t.Fatalf("duration timeout: %v", p.g.Timeout)
	}
	p, _ = parseGlobal([]string{"--timeout", "7", "status"})
	if p.g.Timeout != 7*time.Second {
		t.Fatalf("numeric timeout: %v", p.g.Timeout)
	}
}

func TestRunWithHelp(t *testing.T) {
	t.Setenv("MILLIVOLT_URL", "")
	t.Setenv("NO_COLOR", "1")
	var out, errb bytes.Buffer
	if code := runWith([]string{"help"}, &out, &errb); code != 0 {
		t.Fatalf("help exit %d: %s", code, errb.String())
	}
	for _, want := range []string{"mvctl", "status", "config", "purge"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help missing %q: %s", want, out.String())
		}
	}
}

func TestRunWithVersionAndNoTokenLeak(t *testing.T) {
	t.Setenv("MILLIVOLT_URL", "")
	var out, errb bytes.Buffer
	if code := runWith([]string{"--version"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "mvctl") {
		t.Fatalf("version: %d %s", code, out.String())
	}
	out.Reset()
	code := runWith([]string{"--token", "supersecret", "--json", "url"}, &out, &errb)
	if code != 0 {
		t.Fatalf("url exit %d: %s", code, errb.String())
	}
	if strings.Contains(out.String(), "supersecret") {
		t.Fatal("token leaked")
	}
}

func TestRunWithUnknownCommand(t *testing.T) {
	t.Setenv("MILLIVOLT_URL", "")
	var out, errb bytes.Buffer
	if code := runWith([]string{"not-a-command"}, &out, &errb); code == 0 {
		t.Fatal("unknown command must fail")
	}
}

func TestBooleanFlagsRespectFalseValues(t *testing.T) {
	t.Setenv("MILLIVOLT_URL", "")
	p, err := parseGlobal([]string{"--json=false", "--no-color=0", "--yes=no", "status"})
	if err != nil {
		t.Fatal(err)
	}
	if p.g.JSON || !p.g.Color || p.g.Yes {
		t.Fatalf("false values ignored: %+v", p.g)
	}
	if len(p.args) != 1 || p.args[0] != "status" {
		t.Fatalf("args: %v", p.args)
	}
}

func TestDoubleDashPassthrough(t *testing.T) {
	t.Setenv("MILLIVOLT_URL", "")
	p, err := parseGlobal([]string{"query", "--", "SELECT", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.args) != 3 || p.args[1] != "SELECT" || p.args[2] != "--json" {
		t.Fatalf("args: %v", p.args)
	}
	if p.g.JSON {
		t.Fatal("tokens after -- must not be parsed as flags")
	}
}

func TestNoColorEnvironment(t *testing.T) {
	t.Setenv("MILLIVOLT_URL", "")
	t.Setenv("NO_COLOR", "1")
	p, _ := parseGlobal([]string{"status"})
	if p.g.Color {
		t.Fatal("NO_COLOR must disable colour")
	}
	_ = os.Getenv("NO_COLOR")
}
