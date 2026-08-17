// Package cli defines the demonolith command tree: refactor (code only),
// diff (the sync gate), migrate (state only), and prove (the zero-diff
// proof), connected by the manifest. One command per side of the code/state
// line.
package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// version and commit are injected from main via SetVersion at startup.
var (
	version = "dev"
	commit  = "none"
)

// SetVersion records the build version/commit (set by main from -ldflags).
func SetVersion(v, c string) {
	version, commit = v, c
}

func toolString() string {
	return "demonolith " + version
}

// Exit codes, uniform across commands: 0 success, 1 operational error, 2 a
// negative verdict — the run worked but the answer is "no" (the committed output differs, a module
// plans changes, a stale or inapplicable manifest). Pipelines can therefore
// distinguish "the split is wrong" from "the job broke".
const (
	ExitOK      = 0
	ExitError   = 1
	ExitVerdict = 2
)

// VerdictError marks a negative verdict (exit 2).
type VerdictError struct{ msg string }

func (e *VerdictError) Error() string { return e.msg }

// verdictf builds a negative-verdict error.
func verdictf(format string, a ...any) error {
	return &VerdictError{msg: fmt.Sprintf(format, a...)}
}

// ExitCode maps an Execute error to the process exit code.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var v *VerdictError
	if errors.As(err, &v) {
		return ExitVerdict
	}
	return ExitError
}

// Root builds the root command.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:           "demonolith",
		Short:         "Refactor a monolithic Terraform root into per-module roots",
		Version:       fmt.Sprintf("%s (%s)", version, commit),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(refactorCmd())
	root.AddCommand(diffCmd())
	root.AddCommand(migrateCmd())
	root.AddCommand(proveCmd())
	return root
}

// outputMode is the report format shared by all commands.
type outputMode string

const (
	outputText outputMode = "text"
	outputJSON outputMode = "json"
)

func parseOutput(s string) (outputMode, error) {
	switch outputMode(s) {
	case outputText, outputJSON:
		return outputMode(s), nil
	}
	return "", fmt.Errorf("invalid --output %q (want text or json)", s)
}

// engineExecPath resolves the binary for --engine/--exec-path. --engine has no
// default: the terraform-vs-tofu choice must be explicit.
func engineExecPath(engine, execPath string) (string, error) {
	if execPath != "" {
		return execPath, nil
	}
	switch engine {
	case "terraform", "tofu":
		return exec.LookPath(engine)
	case "":
		return "", fmt.Errorf("--engine is required (terraform or tofu), or pass --exec-path")
	}
	return "", fmt.Errorf("invalid --engine %q (want terraform or tofu)", engine)
}

// resolveRoot makes the --root-dir flag absolute (default current directory).
// Downstream paths derived from it are handed to tfexec instances rooted at
// module dirs, where a relative path would resolve against the wrong base.
func resolveRoot(rootDir string) string {
	if rootDir == "" {
		rootDir = "."
	}
	if abs, err := filepath.Abs(rootDir); err == nil {
		return abs
	}
	return filepath.Clean(rootDir)
}

// stdinIsTTY reports whether stdin is an interactive terminal.
func stdinIsTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// outln / outf write progress output to stdout, ignoring the write error: this
// is best-effort CLI reporting where a failed stdout write is not actionable
// and must not mask the command's real result.
func outln(a ...any)               { _, _ = fmt.Fprintln(os.Stdout, a...) }
func outf(format string, a ...any) { _, _ = fmt.Fprintf(os.Stdout, format, a...) }

// displayPath renders p relative to base when p is under base, else absolute.
func displayPath(base, p string) string {
	rel, err := filepath.Rel(base, p)
	if err != nil || len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
		return p
	}
	return rel
}
