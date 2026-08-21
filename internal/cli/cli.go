// Package cli defines the demonolith command tree: two families split at the
// code/state line, connected by the manifest. refactor map/run/verify carve
// the code; migrate map/prove/run/verify carve, prove, execute, and judge the
// state migration. The bare family commands run their steps in order.
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
// negative verdict — the run worked but the answer is "no" (the split on disk
// differs from the source, a module plans changes, a stale or inapplicable
// manifest). Pipelines can therefore
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
	var noColor bool
	root := &cobra.Command{
		Use:           "demonolith",
		Short:         "Split a monolithic Terraform root into standalone per-module directories",
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
	root.AddCommand(refactorCmd())
	root.AddCommand(migrateCmd())
	return root
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

// resolveOut resolves the --out flag: default under the root, a relative path
// resolved against the root (not the process cwd), and always inside the root —
// the manifest records the output dir root-relative, so an outside dir would
// force an absolute path into it and break every other checkout.
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
