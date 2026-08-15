// Command demonolith refactors a monolithic Terraform/OpenTofu root into
// independent per-module roots, in two halves connected by a manifest:
// `refactor` carves the code and writes the plan, `migrate` executes the state
// moves against local copies, and `verify` proves the split inert via a
// graph-threaded zero-diff plan bundle.
//
// Exit codes: 0 success, 1 operational error, 2 a negative verdict (drift,
// a failed proof, a stale manifest).
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
		os.Exit(cli.ExitCode(err))
	}
}
