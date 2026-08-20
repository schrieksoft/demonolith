package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/schrieksoft/demonolith/internal/bootstrap"
	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/manifest"
	"github.com/schrieksoft/demonolith/internal/pipeline"
	"github.com/schrieksoft/demonolith/internal/proof"
)

// refactorFlags is the union of the refactor subcommands' flags; the bare
// parent pipeline takes them all.
type refactorFlags struct {
	rootDir     string
	out         string
	remainder   string
	interactive bool
	monorepo    bool
	noBootstrap bool
	noBackend   bool
	quiet       bool
	silent      bool
	engine      string
	execPath    string
	overwrite   bool
	yes         bool
}

func refactorCmd() *cobra.Command {
	var f refactorFlags
	cmd := &cobra.Command{
		Use:   "refactor",
		Short: "Split the monolith's code: map → run → validate → diff",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRefactorPipeline(cmd.Context(), f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.out, "out", "", "directory the new module directories are written to, resolved against --root-dir and required to be inside it (default `modules`)")
	flags.StringVar(&f.remainder, "remainder-module", "legacy", "catchall module name for unannotated blocks")
	flags.BoolVar(&f.monorepo, "monorepo", false, "link in-repo child modules by relative path instead of copying them")
	flags.BoolVar(&f.noBootstrap, "no-bootstrap", false, "skip the Snap CD bootstrap module")
	flags.BoolVar(&f.noBackend, "no-backend", false, "skip backend derivation: write the modules without backend blocks")
	flags.StringVar(&f.engine, "engine", "", "engine for the validate step: terraform or tofu (omitted: validate is skipped with a hint)")
	flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	flags.BoolVar(&f.overwrite, "overwrite", false, "delete existing target module directories entirely and rewrite them (everything inside is lost, engine artifacts and any local state included); default refuses when a target directory already has files")
	flags.BoolVarP(&f.interactive, "interactive", "i", false, "guided walkthrough of the whole pipeline")
	flags.BoolVarP(&f.yes, "yes", "y", false, "approve the plan automatically instead of pausing for confirmation before run")

	cmd.AddCommand(refactorMapCmd(), refactorRunCmd(), refactorValidateCmd(), refactorDiffCmd())
	return cmd
}

func refactorMapCmd() *cobra.Command {
	var f refactorFlags
	cmd := &cobra.Command{
		Use:   "map",
		Short: "Analyze the monolith and write the map of the split — the reviewable plan; no module directories are written yet",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.interactive {
				return runRefactorMapInteractive(f)
			}
			_, err := runRefactorMap(f)
			return err
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.out, "out", "", "directory the new module directories are written to, resolved against --root-dir and required to be inside it (default `modules`)")
	flags.StringVar(&f.remainder, "remainder-module", "legacy", "catchall module name for unannotated blocks")
	flags.BoolVar(&f.monorepo, "monorepo", false, "link in-repo child modules by relative path instead of copying them")
	flags.BoolVar(&f.noBootstrap, "no-bootstrap", false, "skip the Snap CD bootstrap module")
	flags.BoolVar(&f.noBackend, "no-backend", false, "skip backend derivation: write the modules without backend blocks")
	flags.BoolVarP(&f.interactive, "interactive", "i", false, "guided walkthrough: prompt for parameters, triage the catchall, confirm before writing the map")
	return cmd
}

func refactorRunCmd() *cobra.Command {
	var f refactorFlags
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Execute the map: write the new module directories; finalize the checksum",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRefactorRun(resolveRoot(f.rootDir), f.overwrite)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.BoolVar(&f.overwrite, "overwrite", false, "delete existing target module directories entirely and rewrite them (everything inside is lost, engine artifacts and any local state included); default refuses when a target directory already has files")
	return cmd
}

// runRefactorMap analyzes and writes the planned manifest. Nothing is emitted;
// backend-type support and the reserved bootstrap name are checked here.
// Whether the target dirs may exist is run's own gate (--overwrite).
func runRefactorMap(f refactorFlags) (*manifest.Manifest, error) {
	rootDir := resolveRoot(f.rootDir)
	outDir, err := resolveOut(rootDir, f.out)
	if err != nil {
		return nil, err
	}

	a, err := pipeline.Analyze(rootDir, pipeline.Options{Remainder: f.remainder})
	if err != nil {
		return nil, err
	}

	opts := manifest.BuildOpts{Monorepo: f.monorepo, Bootstrap: !f.noBootstrap}
	if !f.noBackend {
		block, err := emit.ParseBackend(rootDir)
		if err != nil {
			return nil, err
		}
		if block != nil {
			mono, byModule, err := block.DerivedLocations(a.Placement.ModuleNames())
			if err != nil {
				return nil, err
			}
			opts.Backend = &manifest.Backend{Type: block.Type, Monolith: mono, Modules: byModule}
		}
	}

	if err := checkReservedNames(a, opts.Bootstrap); err != nil {
		return nil, err
	}

	m := manifest.BuildPlanned(a, rootDir, outDir, time.Now(), toolString(), opts)
	path := manifest.Path(rootDir)
	if err := manifest.Write(m, path); err != nil {
		return nil, err
	}

	reportAnalysis(a)
	outln("\n" + heading("Planned module directories:"))
	for _, name := range a.Placement.ModuleNames() {
		outf("  %-16s %s\n", name, m.Modules[name].Dir)
	}
	if m.Backend != nil {
		outf("\n%s\n", heading(fmt.Sprintf("State locations (%s backend, derived from %s):", m.Backend.Type, m.Backend.Monolith)))
		for _, name := range a.Placement.ModuleNames() {
			outf("  %-16s %s\n", name, m.Backend.Modules[name])
		}
	}
	if opts.Bootstrap {
		outf("\nBootstrap module planned at %s\n", filepath.Join(m.Output.Dir, bootstrap.DirName))
	}
	outln("\n" + heading("Receipt:"))
	outf("  %s %s\n", displayPath(rootDir, path), dim(fmt.Sprintf("(%d state moves, %d cross edges)", len(m.StateMoves), len(m.CrossEdges))))
	if !f.interactive {
		outln("\nReview it, then `demonolith refactor run`.")
	}
	outln()
	return m, nil
}

// runTargets is the full set of directories the map claims for emit: every
// module directory plus the bootstrap's. refactor run owns them entirely.
func runTargets(m *manifest.Manifest, rootDir string) []string {
	dirs := m.ModuleDirs(rootDir)
	names := make([]string, 0, len(dirs))
	for name := range dirs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names)+1)
	for _, name := range names {
		out = append(out, dirs[name])
	}
	if m.Output.Bootstrap {
		out = append(out, filepath.Join(m.OutDir(rootDir), bootstrap.DirName))
	}
	return out
}

// existingRunTargets lists the map's emit destinations that already hold
// files, as display paths.
func existingRunTargets(rootDir string) ([]string, error) {
	m, err := manifest.Load(manifest.Path(rootDir))
	if err != nil {
		return nil, err
	}
	var existing []string
	for _, d := range runTargets(m, rootDir) {
		if dirHasFiles(d) {
			existing = append(existing, displayPath(rootDir, d))
		}
	}
	return existing, nil
}

// runRefactorRun executes the planned manifest verbatim: emit the carved roots
// (and backends and bootstrap per the plan), then finalize the checksum. The
// source must still match the plan.
func runRefactorRun(rootDir string, overwrite bool) error {
	path := manifest.Path(rootDir)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no %s found in %s; run `demonolith refactor map` first", manifest.FileName, rootDir)
	}
	m, err := manifest.Load(path)
	if err != nil {
		return err
	}
	outDir := m.OutDir(rootDir)

	a, err := pipeline.Analyze(rootDir, pipeline.Options{Remainder: m.Source.RemainderModule})
	if err != nil {
		return err
	}
	fresh, err := freshSemantic(a, rootDir, m)
	if err != nil {
		return err
	}
	if !manifest.SemanticEqual(fresh, m) {
		return verdictf("the source no longer matches the map; re-run `demonolith refactor map`")
	}

	var block *emit.BackendBlock
	if m.Backend != nil {
		block, err = emit.ParseBackend(rootDir)
		if err != nil {
			return err
		}
		if block == nil {
			return verdictf("the map derives backends but the source has no backend block; re-run `demonolith refactor map`")
		}
	}

	// run owns the target directories entirely: they must not exist, or
	// --overwrite deletes them whole. Hand-added files never survive a run.
	var existing []string
	targets := runTargets(m, rootDir)
	for _, d := range targets {
		if dirHasFiles(d) {
			existing = append(existing, displayPath(rootDir, d))
		}
	}
	if len(existing) > 0 {
		if !overwrite {
			return fmt.Errorf("target module directories already exist: %s — refactor run owns them entirely: delete them, or pass --overwrite to delete and rewrite them (everything inside is lost, engine artifacts and any local state included)", strings.Join(existing, ", "))
		}
		fmt.Fprintf(os.Stderr, "%s\n\n", warn(fmt.Sprintf("WARNING: --overwrite: deleting existing module directories and everything in them: %s", strings.Join(existing, ", "))))
		for _, d := range targets {
			if err := os.RemoveAll(d); err != nil {
				return err
			}
		}
	}

	e := &emit.Emitter{SrcDir: rootDir, OutDir: outDir, Graph: a.Graph, Place: a.Placement, Bound: a.Boundary, Monorepo: m.Output.Monorepo, Backend: block}
	ems, err := e.Emit()
	if err != nil {
		return err
	}

	bsDir := ""
	if m.Output.Bootstrap {
		bsDir, err = bootstrap.Emit(m, rootDir, outDir)
		if err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	}
	if err := m.Finalize(rootDir, bsDir); err != nil {
		return err
	}
	if err := manifest.Write(m, path); err != nil {
		return err
	}

	outln(heading("Module directories written:"))
	for _, em := range ems {
		outf("  %-16s %s (%d files)\n", em.Module, displayPath(rootDir, em.Dir), len(em.Files))
	}
	if bsDir != "" {
		outf("  %-16s %s (Snap CD bootstrap)\n", bootstrap.DirName, displayPath(rootDir, bsDir))
	}
	outln("\n" + heading("Receipt:"))
	outf("  %s %s\n\n", displayPath(rootDir, path), dim("(finalized)"))
	return nil
}

// freshSemantic builds the semantic manifest the current source produces, for
// comparison against the committed one. Backend derivation mirrors the
// committed manifest's choice: compared only when the plan carries one.
func freshSemantic(a *pipeline.Analysis, rootDir string, committed *manifest.Manifest) (*manifest.Manifest, error) {
	fresh := manifest.FromAnalysis(a)
	if committed.Backend != nil {
		block, err := emit.ParseBackend(rootDir)
		if err != nil {
			return nil, err
		}
		if block != nil {
			mono, byModule, err := block.DerivedLocations(a.Placement.ModuleNames())
			if err != nil {
				return nil, err
			}
			fresh.Backend = &manifest.Backend{Type: block.Type, Monolith: mono, Modules: byModule}
		}
	}
	return fresh, nil
}

// runRefactorPipeline is the bare `demonolith refactor`: map → run →
// validate → diff. Validate needs an engine, so without --engine/--exec-path
// it is skipped with a hint rather than failing the pipeline.
func runRefactorPipeline(ctx context.Context, f refactorFlags) error {
	if f.interactive {
		return runRefactorInteractivePipeline(ctx, f)
	}
	outf("\n%s\n\n", banner("── refactor map ──"))
	mf := f
	mf.interactive = true // suppress the standalone "review it, then run" hint
	if _, err := runRefactorMap(mf); err != nil {
		return err
	}
	if !f.yes {
		if !stdinIsTTY() {
			return fmt.Errorf("the pipeline pauses for approval after plan; pass -y to approve automatically, or run the subcommands individually")
		}
		ok, err := promptYesNo("Run the refactor now?", false)
		if err != nil {
			return err
		}
		if !ok {
			outln("Map written; run later with `demonolith refactor run`.")
			return nil
		}
		outln()
	}
	rootDir := resolveRoot(f.rootDir)
	outf("%s\n\n", banner("── refactor run ──"))
	if err := runRefactorRun(rootDir, f.overwrite); err != nil {
		return err
	}
	outf("%s\n\n", banner("── refactor validate ──"))
	if err := pipelineValidate(ctx, rootDir, f); err != nil {
		return err
	}
	outf("%s\n\n", banner("── refactor diff ──"))
	vf := f
	vf.quiet = true
	return runRefactorDiff(rootDir, vf)
}

// pipelineValidate is the validate step inside the bare and interactive
// pipelines: run when an engine is named, otherwise say how to run it.
func pipelineValidate(ctx context.Context, rootDir string, f refactorFlags) error {
	if f.engine == "" && f.execPath == "" {
		outln("Skipped: no engine given. Before committing, have the engine check the new module directories (credential-free) with:")
		outln("  demonolith refactor validate --engine {terraform|tofu}")
		outln()
		return nil
	}
	return runRefactorValidate(ctx, rootDir, f)
}

// checkReservedNames refuses a carved module named after the bootstrap dir.
// Whether target dirs may exist is run's decision (they must be gone, or
// --overwrite deletes them), so map performs no existence checks.
func checkReservedNames(a *pipeline.Analysis, withBootstrap bool) error {
	if !withBootstrap {
		return nil
	}
	for _, name := range a.Placement.ModuleNames() {
		if name == bootstrap.DirName {
			return fmt.Errorf("module name %q is reserved for the Snap CD bootstrap module; rename the module or pass --no-bootstrap", bootstrap.DirName)
		}
	}
	return nil
}

// dirHasFiles reports whether any regular file exists anywhere under dir.
func dirHasFiles(dir string) bool {
	found := false
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// reportAnalysis prints the placement, catchall, and ordering-edge summary.
func reportAnalysis(a *pipeline.Analysis) {
	outln(heading("Placement:"))
	for _, m := range a.Placement.ModuleNames() {
		outf("  %-16s %d resources/data\n", m, len(a.Placement.Modules[m]))
	}
	if len(a.Placement.Catchall) > 0 {
		outf("\n%s\n", heading(fmt.Sprintf("Catchall (%s) holds %d unannotated block(s):", a.Placement.Remainder, len(a.Placement.Catchall))))
		for _, addr := range a.Placement.Catchall {
			outf("  %s\n", addr)
		}
	}
	if order, err := proof.TopoOrder(a.Placement.ModuleNames(), a.Boundary); err == nil {
		deps := proof.ModuleDeps(a.Placement.ModuleNames(), a.Boundary)
		outln("\n" + heading("A dependency graph arises with the following deploy order:"))
		for _, m := range order {
			if d := deps[m]; len(d) > 0 {
				outf("  %-16s %s\n", m, dim("(depends on: "+strings.Join(d, ", ")+")"))
			} else {
				outf("  %s\n", m)
			}
		}
	}
}
