// Command demonolith refactors a monolithic Terraform/OpenTofu root into
// independent per-module roots. v1 is a one-shot splitter: it emits carved
// roots (detached — no Snap CD wiring), carves state into per-module local
// files against local copies (never pushing), and can prove the split changes
// nothing via a graph-threaded zero-diff plan bundle.
package main

import (
	"fmt"
	"os"

	"github.com/schrieksoft/demonolith/internal/cli"
)

// Populated at build time by GoReleaser via -ldflags.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	cli.SetVersion(version, commit)
	if err := cli.Root().Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
