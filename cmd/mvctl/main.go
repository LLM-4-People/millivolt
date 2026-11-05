// Command mvctl is a small human client over millivolt's existing operator
// plane. It adds no server endpoints, manages no containers and depends only
// on the Go standard library. It is the command-line counterpart to the /mcp
// tools and the dashboard Settings sheet.

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const versionString = "mvctl (millivolt operator CLI)"

var boolFlags = map[string]bool{
	"json": true, "no-color": true, "color": true, "yes": true,
	"verbose": true, "v": true, "help": true, "h": true, "version": true,
}

var valueFlags = map[string]bool{
	"url": true, "token": true, "timeout": true,
}

type parsed struct {
	g           Global
	args        []string
	flags       map[string]string
	showHelp    bool
	showVersion bool
}

func main() {
	os.Exit(runWith(os.Args[1:], os.Stdout, os.Stderr))
}

func runWith(argv []string, stdout, stderr io.Writer) int {
	p, err := parseGlobal(argv)
	if err != nil {
		fmt.Fprintln(stderr, "mvctl: "+err.Error())
		return 2
	}
	setColor(p.g.Color)
	if p.showVersion {
		fmt.Fprintln(stdout, versionString)
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmds := CoreCommands()
	// --help short-circuits before any command runs, regardless of position.
	if p.showHelp {
		fmt.Fprintln(stdout, Help(cmds))
		return 0
	}
	c := NewClient(p.g)
	result, err := Dispatch(cmds, Context{
		Ctx: ctx, G: p.g, C: c, Args: p.args, Flags: p.flags,
	})
	if err != nil {
		fmt.Fprintln(stderr, "mvctl: "+err.Error())
		if !p.g.JSON && result.Pretty != "" {
			fmt.Fprintln(stdout, result.Pretty)
		}
		if p.g.JSON {
			payload := map[string]any{"error": err.Error()}
			if result.Data != nil {
				payload["details"] = result.Data
			}
			fmt.Fprintln(stdout, toJSON(payload))
		}
		if result.Code != 0 {
			return result.Code
		}
		return 1
	}
	if p.g.JSON && result.Data != nil {
		fmt.Fprintln(stdout, toJSON(result.Data))
	} else if result.Pretty != "" {
		fmt.Fprintln(stdout, result.Pretty)
	}
	return result.Code
}

func parseGlobal(argv []string) (parsed, error) {
	out := parsed{
		g:     Global{Color: true, Timeout: 10 * time.Second},
		flags: map[string]string{},
	}
	out.g.BaseURL = envOr("MILLIVOLT_URL", "http://127.0.0.1:8080")
	out.g.Token = os.Getenv("MILLIVOLT_OPERATOR_TOKEN")
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			out.args = append(out.args, argv[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "--") {
			body := a[2:]
			name, val := body, ""
			if eq := strings.Index(body, "="); eq >= 0 {
				name, val = body[:eq], body[eq+1:]
			}
			if boolFlags[name] {
				enabled := boolFlagValue(val)
				switch name {
				case "json":
					out.g.JSON = enabled
				case "no-color":
					out.g.Color = !enabled
				case "color":
					out.g.Color = enabled
				case "yes":
					out.g.Yes = enabled
				case "verbose", "v":
					out.g.Verbose = enabled
				case "help", "h":
					out.showHelp = enabled
				case "version":
					out.showVersion = enabled
				}
				continue
			}
			if val == "" && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				if valueFlags[name] && knownCommand(argv[i+1]) {
					return out, fmt.Errorf("missing value for --%s", name)
				}
				val = argv[i+1]
				i++
			}
			if val == "" {
				val = "true"
			}
			if valueFlags[name] {
				switch name {
				case "url":
					out.g.BaseURL = strings.TrimRight(val, "/")
				case "token":
					out.g.Token = val
				case "timeout":
					if d, err := time.ParseDuration(val); err == nil {
						out.g.Timeout = d
					} else if n, err := strconv.Atoi(val); err == nil {
						out.g.Timeout = time.Duration(n) * time.Second
					}
				}
				out.flags[name] = val
				continue
			}
			out.flags[name] = val
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			for _, letter := range a[1:] {
				switch letter {
				case 'j':
					out.g.JSON = true
				case 'h':
					out.showHelp = true
				case 'v':
					out.g.Verbose = true
				default:
					return out, fmt.Errorf("unknown short flag -%c", letter)
				}
			}
			continue
		}
		out.args = append(out.args, a)
	}
	if out.g.Color && (os.Getenv("NO_COLOR") != "" || os.Getenv("MVCTL_NO_COLOR") != "") {
		out.g.Color = false
	}
	return out, nil
}

// knownCommand prevents a value flag from swallowing the command word.
func knownCommand(word string) bool {
	set := map[string]bool{}
	for _, c := range CoreCommands() {
		set[c.Name] = true
	}
	return set[word] || word == "help"
}

// boolFlagValue treats an omitted value as true and recognises common false
// spellings, so --json=false and --no-color=0 behave as expected.
func boolFlagValue(val string) bool {
	if val == "" {
		return true
	}
	switch strings.ToLower(val) {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
