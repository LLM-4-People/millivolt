// Command release validates release metadata and writes GitHub output lines.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/LLM-4-People/millivolt"
)

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("release", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tag := flags.String("tag", "", "optional release tag; must equal v plus VERSION")
	revision := flags.String("revision", "", "full source commit ID for release metadata")
	newerThan := flags.String("newer-than", "", "require VERSION to be newer than this release version")
	newest := flags.Bool("newest", false, "print the newest release version read from standard input")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	emptyTag := false
	flags.Visit(func(f *flag.Flag) { emptyTag = emptyTag || f.Name == "tag" && f.Value.String() == "" })
	if emptyTag {
		return fmt.Errorf("release tag must not be empty when supplied")
	}
	if *newest {
		if *tag != "" || *revision != "" || *newerThan != "" {
			return fmt.Errorf("-newest cannot be combined with -tag, -revision or -newer-than")
		}
		return newestVersion(stdin, stdout, stderr)
	}
	if *newerThan != "" {
		if *tag != "" || *revision != "" {
			return fmt.Errorf("-newer-than cannot be combined with -tag or -revision")
		}
		comparison, err := millivolt.CompareVersions(millivolt.Version(), *newerThan)
		if err != nil {
			return err
		}
		if comparison <= 0 {
			return fmt.Errorf("VERSION %s must be newer than %s", millivolt.Version(), *newerThan)
		}
		_, err = fmt.Fprintf(stdout, "version=%s\nnewer_than=%s\n", millivolt.Version(), *newerThan)
		return err
	}
	info, err := millivolt.ReleaseMetadata(*tag, *revision)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "version=%s\nprerelease=%t\nrevision=%s\n", info.Version, info.Prerelease, info.Revision)
	return err
}

// newestVersion is the single owner of "which release version is the newest".
// It reads candidate versions (one per line, with or without a leading v) from
// stdin, ignores anything that is not a release version after reporting it on
// stderr, and prints the highest by SemVer precedence. An empty input prints
// nothing, so a first release has no predecessor.
//
// The release workflow uses it instead of comparing tag names itself: a stray
// tag such as `v1.0` or a vendor tag must never block a promotion, and a
// backport released after a newer version must not become the latest release.
func newestVersion(input io.Reader, stdout, stderr io.Writer) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 0, 64), 4096)
	best := ""
	for scanner.Scan() {
		candidate := strings.TrimSpace(scanner.Text())
		// A tag-style leading v is part of the tag, not the version. Only strip
		// it before a digit, so a vendor tag such as `vendor-2024` is reported
		// as what it is instead of losing its first letter.
		if len(candidate) > 1 && candidate[0] == 'v' && candidate[1] >= '0' && candidate[1] <= '9' {
			candidate = candidate[1:]
		}
		if candidate == "" {
			continue
		}
		if err := millivolt.ValidateVersion(candidate); err != nil {
			fmt.Fprintf(stderr, "release: ignoring %q: %v\n", candidate, err)
			continue
		}
		if best == "" {
			best = candidate
			continue
		}
		comparison, err := millivolt.CompareVersions(candidate, best)
		if err != nil {
			return err
		}
		if comparison > 0 {
			best = candidate
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if best == "" {
		return nil
	}
	_, err := fmt.Fprintln(stdout, best)
	return err
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}
