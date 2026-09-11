// Package cli defines the demonolith command tree: the split's refactor
// (map/run/validate/diff) and migrate (map/prove/run/verify) families,
// connected by the manifest, plus the transfer family for moves between
// pre-existing roots. The bare family commands run their steps in order.
package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
// negative verdict - the run worked but the answer is "no" (the split on disk
// differs from the source, a module plans changes, a stale manifest).
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
	var noColor bool
	root := &cobra.Command{
		Use:           "demonolith",
		Short:         "Restructure Terraform/OpenTofu roots: split a monolith into per-module roots, or transfer blocks between existing roots",
		Version:       fmt.Sprintf("%s (%s)", version, commit),
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(*cobra.Command, []string) {
			if noColor {
				colorEnabled = false
			}
		},
	}
	root.PersistentFlags().BoolVar(&noColor, "no-color", false, "disable colored output (the NO_COLOR environment variable works too)")
	root.AddCommand(splitCmd())
	legacyRefactor := refactorCmd()
	deprecateTree(legacyRefactor, "use `demonolith split refactor ...` (removed at the latest in v1.0.0)")
	legacyMigrate := migrateCmd()
	deprecateTree(legacyMigrate, "use `demonolith split migrate ...` (removed at the latest in v1.0.0)")
	root.AddCommand(legacyRefactor, legacyMigrate, transferCmd())
	return root
}

// splitCmd names the split: the refactor and migrate families under one
// top-level command, beside transfer.
func splitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "split",
		Short: "Split a monolithic root into per-module roots: refactor (the code split), then migrate (the state migration)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(refactorCmd(), migrateCmd())
	return cmd
}

// deprecateTree marks a command and all its descendants deprecated; cobra
// prints the notice on execution and hides them from help.
func deprecateTree(c *cobra.Command, msg string) {
	c.Deprecated = msg
	for _, sub := range c.Commands() {
		deprecateTree(sub, msg)
	}
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

// resolveOut resolves --out: relative to the root, and always inside it -
// the manifest records the dir root-relative, so an outside dir would force
// an absolute path into it and break other checkouts.
func resolveOut(rootDir, out string) (string, error) {
	if out == "" {
		return filepath.Join(rootDir, "roots"), nil
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(rootDir, out)
	}
	out = filepath.Clean(out)
	rel, err := filepath.Rel(rootDir, out)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("--out %s is outside --root-dir %s; the map records the output dir relative to the root, and an outside dir would make it non-portable", out, rootDir)
	}
	return out, nil
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
