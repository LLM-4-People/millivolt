// Command docgen renders docs/settings.md, the generated reference of every
// user-tunable setting and every proxy CLI flag.
//
//	go run ./cmd/docgen -write   # rewrite the page from the code
//	go run ./cmd/docgen -check   # exit 1 when the page and the code disagree
//
// Run it from the module root: the renderer reads internal/config and parses
// cmd/proxy/main.go.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/LLM-4-People/millivolt/internal/settingsdoc"
)

func main() {
	write := flag.Bool("write", false, "rewrite "+settingsdoc.PagePath+" from internal/config and the proxy flag registration")
	check := flag.Bool("check", false, "re-render in memory and exit 1 when "+settingsdoc.PagePath+" differs")
	flag.Parse()
	if *write == *check || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/docgen -write|-check")
		os.Exit(2)
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "docgen: %v\n", err)
		os.Exit(1)
	}
	if *write {
		if err := settingsdoc.Write(root); err != nil {
			fmt.Fprintf(os.Stderr, "docgen: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("wrote %s\n", settingsdoc.PagePath)
		return
	}
	if err := settingsdoc.Check(root); err != nil {
		fmt.Fprintf(os.Stderr, "docgen: %v\n", err)
		os.Exit(1)
	}
}
