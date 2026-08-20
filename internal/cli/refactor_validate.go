package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/spf13/cobra"
)

func refactorValidateCmd() *cobra.Command {
	var f refactorFlags
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Ask the engine whether the written module directories are valid (init -backend=false + validate; credential-free)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRefactorValidate(cmd.Context(), resolveRoot(f.rootDir), f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.engine, "engine", "", "engine: terraform or tofu (required)")
	flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	flags.BoolVarP(&f.quiet, "quiet", "q", false, "verdict only; skip the per-module listing")
	flags.BoolVar(&f.silent, "silent", false, "no output at all; the result is the exit code")
	return cmd
}

// validateReport collects the engine's verdicts for reporting.
type validateReport struct {
	Valid          bool
	Modules        []string
	InvalidModules []string
	Diagnostics    []string
}

// runRefactorValidate runs `init -backend=false` + `validate` on each written
// module directory (bootstrap included): engine-grade validity — providers
// installed, references resolved, types checked — without touching any state
// backend. Only the provider registry is contacted.
func runRefactorValidate(ctx context.Context, rootDir string, f refactorFlags) error {
	if ctx == nil {
		ctx = context.Background()
	}
	execPath, err := engineExecPath(f.engine, f.execPath)
	if err != nil {
		return err
	}
	m, err := loadRunManifest(rootDir)
	if err != nil {
		return err
	}

	rep := validateReport{}
	dirs := m.ChecksumDirs(rootDir)
	names := make([]string, 0, len(dirs))
	for name := range dirs {
		names = append(names, name)
	}
	sort.Strings(names)
	rep.Modules = names

	verbose := !f.quiet && !f.silent
	for _, name := range names {
		if verbose {
			outf("  %s: validating ... ", name)
		}
		tf, err := tfexec.NewTerraform(dirs[name], execPath)
		if err != nil {
			return err
		}
		if err := tf.Init(ctx, tfexec.Backend(false)); err != nil {
			return fmt.Errorf("init %s: %w", name, err)
		}
		out, err := tf.Validate(ctx)
		if err != nil {
			return fmt.Errorf("validate %s: %w", name, err)
		}
		if out.Valid {
			if verbose {
				outf("%s\n", success("valid"))
			}
			continue
		}
		rep.InvalidModules = append(rep.InvalidModules, name)
		for _, d := range out.Diagnostics {
			rep.Diagnostics = append(rep.Diagnostics, fmt.Sprintf("%s: %s: %s", name, d.Severity, d.Summary))
		}
		if verbose {
			outf("%s\n", fail("invalid"))
			for _, d := range out.Diagnostics {
				outf("    %s: %s\n", d.Severity, d.Summary)
			}
		}
	}
	rep.Valid = len(rep.InvalidModules) == 0
	return reportRefactorValidate(rep, f)
}

func reportRefactorValidate(rep validateReport, f refactorFlags) error {
	if f.silent {
		if !rep.Valid {
			return &VerdictError{}
		}
		return nil
	}
	if rep.Valid {
		if !f.quiet {
			outln()
		}
		outf("%s: the engine accepts all %d module directories.\n\n", success("Valid"), len(rep.Modules))
	} else if f.quiet {
		for _, d := range rep.Diagnostics {
			outf("  %s\n", d)
		}
	}
	if !rep.Valid {
		return verdictf("the engine rejects module directory(ies): %s", strings.Join(rep.InvalidModules, ", "))
	}
	return nil
}
