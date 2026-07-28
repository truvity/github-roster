// Command github-roster runs the roster console.
package main

import (
	"fmt"
	"os"

	"github.com/truvity/github-roster/pkg/version"
)

// Injected at release time by goreleaser's ldflags.
var (
	Version = "dev"
	Commit  = ""
)

func main() {
	info := version.Info{Version: Version, Commit: Commit}

	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(info.String())

		return
	}

	fmt.Fprintf(os.Stderr, "github-roster %s: no server yet — see the phase plan in docs/\n", info.String())
	os.Exit(1)
}
