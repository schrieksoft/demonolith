package cli

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/spf13/cobra"

	"github.com/schrieksoft/demonolith/internal/bootstrap"
	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/manifest"
	"github.com/schrieksoft/demonolith/internal/pipeline"
)

func refactorVerifyCmd() *cobra.Command {
	var f refactorFlags
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Provenance gate: do the committed roots and manifest match the committed source?",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := parseOutput(f.output)
			if err != nil {
				return err
			}
			return runRefactorVerify(resolveRoot(f.rootDir), mode, f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.output, "output", "text", "report format: text or json")
	flags.BoolVarP(&f.quiet, "quiet", "q", false, "verdict only; skip the per-module placement listing")
	flags.BoolVar(&f.silent, "silent", false, "no output at all; the result is the exit code")
	flags.BoolVar(&f.validate, "validate", false, "also engine-validate each committed root (init -backend=false + validate; credential-free, needs --engine)")
	flags.StringVar(&f.engine, "engine", "", "engine for --validate: terraform or tofu")
	flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	return cmd
}

// verifyReport is the machine-facing result of refactor verify.
type verifyReport struct {
	Differs        bool     `json:"differs"`
	Manifest       string   `json:"manifest,omitempty"`
	ChangedFiles   []string `json:"changed_files,omitempty"`
	Reasons        []string `json:"reasons,omitempty"`
	InvalidModules []string `json:"invalid_modules,omitempty"`
}

// runRefactorVerify re-runs analysis+emit into a temp dir and compares against
// the committed roots and manifest. The remainder-module name comes from the
// committed manifest, so no placement flag can skew the comparison. Nothing is
// written to the root or output dirs; any mismatch is a negative verdict.
func runRefactorVerify(rootDir string, mode outputMode, f refactorFlags) error {
	rep := verifyReport{}
	path := manifest.Path(rootDir)
	if _, err := os.Stat(path); err != nil {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, "no committed "+manifest.FileName+" found; run `demonolith refactor plan` and commit its output")
		return reportRefactorVerify(rep, nil, mode, f)
	}
	rep.Manifest = manifest.FileName
	committed, err := manifest.Load(path)
	if err != nil {
		return err
	}
	if !committed.IsRun() {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, "manifest is planned but not run; run `demonolith refactor run` and commit its output")
		return reportRefactorVerify(rep, committed, mode, f)
	}

	a, err := pipeline.Analyze(rootDir, pipeline.Options{Remainder: committed.Source.RemainderModule})
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "demono-verify-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	var block *emit.BackendBlock
	if committed.Backend != nil {
		block, err = emit.ParseBackend(rootDir)
		if err != nil {
			return err
		}
	}
	e := &emit.Emitter{SrcDir: rootDir, OutDir: tmp, Graph: a.Graph, Place: a.Placement, Bound: a.Boundary, Monorepo: committed.Output.Monorepo, Backend: block}
	ems, err := e.Emit()
	if err != nil {
		return err
	}
	freshDirs := map[string]string{}
	for _, em := range ems {
		freshDirs[em.Module] = em.Dir
	}
	fresh, err := freshSemantic(a, rootDir, committed)
	if err != nil {
		return err
	}
	if committed.Output.BootstrapDir != "" {
		bsDir, err := bootstrap.Emit(committed, rootDir, tmp)
		if err != nil {
			return err
		}
		freshDirs["snapcd-bootstrap"] = bsDir
	}

	committedHashes, err := manifest.FileHashes(committed.ChecksumDirs(rootDir))
	if err != nil {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, fmt.Sprintf("committed roots unreadable: %v", err))
		return reportRefactorVerify(rep, committed, mode, f)
	}
	freshHashes, err := manifest.FileHashes(freshDirs)
	if err != nil {
		return err
	}

	rep.ChangedFiles = diffHashes(freshHashes, committedHashes)
	if len(rep.ChangedFiles) > 0 {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, "committed roots differ from what the committed source produces")
	}
	if committed.EmitChecksum != manifest.ChecksumOf(committedHashes) {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, "committed roots were edited after the manifest was written (emit_checksum mismatch)")
	}
	if !manifest.SemanticEqual(fresh, committed) {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, "manifest content differs from what the committed source produces")
	}

	if f.validate && !rep.Differs {
		invalid, err := validateRoots(committed, rootDir, f)
		if err != nil {
			return err
		}
		if len(invalid) > 0 {
			rep.Differs = true
			rep.InvalidModules = invalid
			rep.Reasons = append(rep.Reasons, "engine validation failed for: "+joinComma(invalid))
		}
	}
	return reportRefactorVerify(rep, committed, mode, f)
}

// validateRoots runs `init -backend=false` + `validate` on each committed root
// (bootstrap included): engine-grade validity, still credential-free.
func validateRoots(m *manifest.Manifest, rootDir string, f refactorFlags) ([]string, error) {
	execPath, err := engineExecPath(f.engine, f.execPath)
	if err != nil {
		return nil, fmt.Errorf("--validate needs an engine: %w", err)
	}
	ctx := context.Background()
	dirs := m.ChecksumDirs(rootDir)
	names := make([]string, 0, len(dirs))
	for name := range dirs {
		names = append(names, name)
	}
	sort.Strings(names)
	var invalid []string
	for _, name := range names {
		tf, err := tfexec.NewTerraform(dirs[name], execPath)
		if err != nil {
			return nil, err
		}
		if err := tf.Init(ctx, tfexec.Backend(false)); err != nil {
			return nil, fmt.Errorf("init %s: %w", name, err)
		}
		out, err := tf.Validate(ctx)
		if err != nil {
			return nil, fmt.Errorf("validate %s: %w", name, err)
		}
		if !out.Valid {
			invalid = append(invalid, name)
			for _, d := range out.Diagnostics {
				outf("  %s: %s: %s\n", name, d.Severity, d.Summary)
			}
		}
	}
	return invalid, nil
}

func joinComma(items []string) string {
	out := ""
	for i, it := range items {
		if i > 0 {
			out += ", "
		}
		out += it
	}
	return out
}

// diffHashes lists "<module>/<relpath>" entries that differ between the fresh
// and committed trees, tagging additions and removals.
func diffHashes(fresh, committed map[string]string) []string {
	var out []string
	for k, fv := range fresh {
		cv, ok := committed[k]
		if !ok {
			out = append(out, k+" (missing from committed roots)")
		} else if cv != fv {
			out = append(out, k)
		}
	}
	for k := range committed {
		if _, ok := fresh[k]; !ok {
			out = append(out, k+" (not produced by source)")
		}
	}
	sort.Strings(out)
	return out
}

func reportRefactorVerify(rep verifyReport, m *manifest.Manifest, mode outputMode, f refactorFlags) error {
	if f.silent {
		// Exit code only: an empty-message verdict that main does not print.
		if rep.Differs {
			return &VerdictError{}
		}
		return nil
	}
	if mode == outputJSON {
		if err := printJSON(rep); err != nil {
			return err
		}
	} else {
		if !f.quiet && m != nil {
			printPlacement(m)
		}
		if rep.Differs {
			outln("Committed output differs from the committed source:")
			for _, r := range rep.Reasons {
				outf("  - %s\n", r)
			}
			for _, fl := range rep.ChangedFiles {
				outf("    %s\n", fl)
			}
		} else {
			outf("In sync: committed roots and manifest %s match the committed source.\n", rep.Manifest)
		}
	}
	if rep.Differs {
		return verdictf("committed output differs from the committed source; re-run `demonolith refactor` and commit its output")
	}
	return nil
}

// printPlacement lists what the committed manifest claims — every block and
// the root it was carved into — so an in-sync verdict shows what was actually
// confirmed.
func printPlacement(m *manifest.Manifest) {
	outln("Committed plan under comparison:")
	names := make([]string, 0, len(m.Modules))
	for name := range m.Modules {
		names = append(names, name)
	}
	sort.Strings(names)
	blocks := 0
	for _, name := range names {
		mod := m.Modules[name]
		outf("  %-16s %s\n", name, mod.Dir)
		for _, b := range mod.Blocks {
			outf("    %s\n", b)
			blocks++
		}
	}
	outf("  (%d modules, %d blocks, %d state moves, %d cross edges)\n\n", len(names), blocks, len(m.StateMoves), len(m.CrossEdges))
}
