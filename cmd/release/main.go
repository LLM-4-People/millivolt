// Command release validates release metadata and writes GitHub output lines.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/LLM-4-People/millivolt"
)

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("release", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tag := flags.String("tag", "", "optional release tag; must equal v plus VERSION")
	revision := flags.String("revision", "", "required full source commit ID")
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
	info, err := millivolt.ReleaseMetadata(*tag, *revision)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "version=%s\nprerelease=%t\nrevision=%s\n", info.Version, info.Prerelease, info.Revision)
	return err
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}
