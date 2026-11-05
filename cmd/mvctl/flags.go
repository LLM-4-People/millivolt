package main

// Command-line parsing. Flags are registered per command, unknown flags are
// rejected before any request, and every default comes from settings.go.

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

type colorMode int

const (
	colorAuto colorMode = iota
	colorAlways
	colorNever
)

// Global is the resolved client configuration. The credential is read from
// mcp.ProxyTokenEnv only: a --token flag would expose it through ps and shell
// history, and no existing millivolt command accepts it on argv.
type Global struct {
	BaseURL string
	Token   string
	JSON    bool
	Yes     bool
	Timeout time.Duration
	Color   colorMode
	// Settings carries the defaults resolved once at startup. Commands never
	// consult internal/config directly.
	Settings settings
}

type flagKind int

const (
	kindBool flagKind = iota
	kindString
	kindInt
	kindDuration
)

type flagSpec struct {
	name  string
	short rune
	kind  flagKind
	usage string
}

type flagValues struct {
	values map[string]any
	set    map[string]bool
}

func (f flagValues) String(name, fallback string) string {
	if v, ok := f.values[name].(string); ok {
		return v
	}
	return fallback
}

func (f flagValues) Bool(name string, fallback bool) bool {
	if v, ok := f.values[name].(bool); ok {
		return v
	}
	return fallback
}

func (f flagValues) Int(name string, fallback int64) int64 {
	if v, ok := f.values[name].(int64); ok {
		return v
	}
	return fallback
}

func (f flagValues) Duration(name string, fallback time.Duration) time.Duration {
	if v, ok := f.values[name].(time.Duration); ok {
		return v
	}
	return fallback
}

func (f flagValues) Present(name string) bool { return f.set[name] }

type invocation struct {
	global  Global
	command *Command
	args    []string
	flags   flagValues
	help    bool
	version bool
}

func globalFlagSpecs(defaults settings) []flagSpec {
	return []flagSpec{
		{name: "json", short: 'j', kind: kindBool, usage: "emit JSON instead of human output"},
		{name: "url", kind: kindString, usage: "proxy origin (required; " + proxyURLEnv + " may supply it)"},
		{name: "timeout", kind: kindDuration, usage: fmt.Sprintf("bound one request (default %s; 0 disables the deadline)", defaults.Timeout)},
		{name: "yes", kind: kindBool, usage: "confirm a state-changing command"},
		{name: "color", kind: kindBool, usage: "force ANSI color (--color=false disables)"},
		{name: "no-color", kind: kindBool, usage: "disable ANSI color"},
		{name: "help", short: 'h', kind: kindBool, usage: "show help"},
		{name: "version", kind: kindBool, usage: "show the mvctl version"},
	}
}

func parseInvocation(argv []string, cmds []Command) (invocation, error) {
	defaults := defaultSettings()
	inv := invocation{
		global: Global{
			BaseURL:  strings.TrimSpace(os.Getenv(proxyURLEnv)),
			Token:    os.Getenv(mcp.ProxyTokenEnv),
			Timeout:  defaults.Timeout,
			Color:    colorAuto,
			Settings: defaults,
		},
		flags: flagValues{values: map[string]any{}, set: map[string]bool{}},
	}
	byName := make(map[string]*Command, len(cmds))
	for i := range cmds {
		byName[cmds[i].Name] = &cmds[i]
	}
	findFlag := func(name string) (flagSpec, bool) {
		if inv.command != nil {
			for _, spec := range inv.command.Flags {
				if spec.name == name {
					return spec, true
				}
			}
		}
		for _, spec := range globalFlagSpecs(defaults) {
			if spec.name == name {
				return spec, true
			}
		}
		return flagSpec{}, false
	}
	findShort := func(letter rune) (flagSpec, bool) {
		if inv.command != nil {
			for _, spec := range inv.command.Flags {
				if spec.short == letter {
					return spec, true
				}
			}
		}
		for _, spec := range globalFlagSpecs(defaults) {
			if spec.short == letter {
				return spec, true
			}
		}
		return flagSpec{}, false
	}

	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			rest := argv[i+1:]
			if inv.command == nil && len(rest) > 0 {
				cmd, ok := byName[rest[0]]
				if !ok {
					return inv, fmt.Errorf("unknown command %q (try 'mvctl help')", rest[0])
				}
				inv.command = cmd
				rest = rest[1:]
			}
			inv.args = append(inv.args, rest...)
			break
		}
		if strings.HasPrefix(arg, "--") {
			name, raw, hasValue := strings.Cut(arg[2:], "=")
			if name == "" {
				return inv, errors.New("empty flag name")
			}
			spec, ok := findFlag(name)
			if !ok {
				return inv, fmt.Errorf("unknown flag --%s", name)
			}
			if spec.kind != kindBool && !hasValue {
				if i+1 >= len(argv) {
					return inv, fmt.Errorf("missing value for --%s", name)
				}
				i++
				raw = argv[i]
			}
			if spec.kind == kindBool && !hasValue {
				raw = "true"
			}
			value, err := parseFlagValue(spec, raw)
			if err != nil {
				return inv, fmt.Errorf("--%s: %w", name, err)
			}
			if err := applyFlag(&inv, name, value); err != nil {
				return inv, err
			}
			inv.flags.values[name] = value
			inv.flags.set[name] = true
			continue
		}
		if strings.HasPrefix(arg, "-") && len(arg) > 1 {
			for _, letter := range arg[1:] {
				spec, ok := findShort(letter)
				if !ok {
					return inv, fmt.Errorf("unknown short flag -%c", letter)
				}
				if spec.kind != kindBool {
					return inv, fmt.Errorf("short flag -%c needs a value; use --%s", letter, spec.name)
				}
				if err := applyFlag(&inv, spec.name, true); err != nil {
					return inv, err
				}
				inv.flags.values[spec.name] = true
				inv.flags.set[spec.name] = true
			}
			continue
		}
		if inv.command == nil {
			if arg == "help" {
				inv.help = true
				continue
			}
			cmd, ok := byName[arg]
			if !ok {
				return inv, fmt.Errorf("unknown command %q (try 'mvctl help')", arg)
			}
			inv.command = cmd
			continue
		}
		inv.args = append(inv.args, arg)
	}
	if inv.command == nil && len(inv.args) > 0 {
		return inv, fmt.Errorf("unknown command %q (try 'mvctl help')", inv.args[0])
	}
	return inv, nil
}

func applyFlag(inv *invocation, name string, value any) error {
	switch name {
	case "json":
		inv.global.JSON = value.(bool)
	case "url":
		inv.global.BaseURL = strings.TrimSpace(value.(string))
	case "timeout":
		inv.global.Timeout = value.(time.Duration)
		if inv.global.Timeout < 0 {
			return errors.New("--timeout must not be negative")
		}
	case "yes":
		inv.global.Yes = value.(bool)
	case "color":
		if value.(bool) {
			inv.global.Color = colorAlways
		} else {
			inv.global.Color = colorNever
		}
	case "no-color":
		if value.(bool) {
			inv.global.Color = colorNever
		} else {
			inv.global.Color = colorAuto
		}
	case "help":
		inv.help = value.(bool)
	case "version":
		inv.version = value.(bool)
	}
	return nil
}

func parseFlagValue(spec flagSpec, raw string) (any, error) {
	switch spec.kind {
	case kindBool:
		value, err := parseBool(raw)
		if err != nil {
			return nil, err
		}
		return value, nil
	case kindString:
		return raw, nil
	case kindInt:
		value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return nil, errors.New("want an integer")
		}
		return value, nil
	case kindDuration:
		value, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return nil, errors.New("want a duration such as 30s or 2m")
		}
		return value, nil
	default:
		return nil, errors.New("unsupported flag kind")
	}
}

// parseBool accepts the spellings an operator is likely to type. An omitted
// value is true; every other spelling must be unambiguous.
func parseBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	default:
		return false, errors.New("want true or false")
	}
}
