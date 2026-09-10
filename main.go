// Command demonolith restructures Terraform/OpenTofu roots without changing
// infrastructure: `refactor`/`migrate` split a monolith into per-module roots,
// and `transfer` moves blocks between pre-existing roots - each a code half
// and a state half connected by a reviewable plan, gated by zero-diff proofs.
//
// Exit codes: 0 success, 1 operational error, 2 negative verdict.
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
		// An empty message is a silent verdict: the exit code is the answer.
		if err.Error() != "" {
			_, _ = fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(cli.ExitCode(err))
	}
}
