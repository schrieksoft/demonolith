// Package cli defines the demonolith command tree.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/pipeline"
	"github.com/schrieksoft/demonolith/internal/proof"
	"github.com/schrieksoft/demonolith/internal/statemove"
	"github.com/schrieksoft/demonolith/internal/statevars"
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

// Root builds the root command.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:           "demono",
		Short:         "Refactor a monolithic Terraform root into per-module roots",
		Version:       fmt.Sprintf("%s (%s)", version, commit),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(splitCmd())
	return root
}

type splitFlags struct {
	out        string
	remainder  string
	engine     string
	execPath   string
	withState  bool
	withProof  bool
	withTfvars bool
	refresh    bool
	statePath  string
}

func splitCmd() *cobra.Command {
	var f splitFlags
	cmd := &cobra.Command{
		Use:   "split <root-dir>",
		Short: "Split a monolith into carved per-module roots",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSplit(cmd.Context(), args[0], f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.out, "out", "", "output directory for carved roots (default <root-dir>/.demono/modules)")
	flags.StringVar(&f.remainder, "remainder-module", "monolith", "catchall module name for unannotated blocks")
	flags.StringVar(&f.engine, "engine", "terraform", "state engine: terraform or tofu")
	flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	flags.BoolVar(&f.withState, "state", false, "carve state into per-module local files (needs a terraform/tofu binary)")
	flags.StringVar(&f.statePath, "state-file", "", "local monolith state file to carve (default: pull from backend)")
	flags.BoolVar(&f.withTfvars, "tfvars", false, "write generated.auto.tfvars into each module, populated from the applied source state (implies --state)")
	flags.BoolVar(&f.withProof, "proof", false, "run the graph-threaded zero-diff proof (implies --state)")
	flags.BoolVar(&f.refresh, "refresh", false, "refresh state during proof (authoritative but needs credentials)")
	return cmd
}

func runSplit(ctx context.Context, srcDir string, f splitFlags) error {
	if ctx == nil {
		ctx = context.Background()
	}
	srcDir = filepath.Clean(srcDir)

	// 1. Analyze: parse -> decorators -> placement -> boundary -> cycle gate.
	a, err := pipeline.Analyze(srcDir, pipeline.Options{Remainder: f.remainder})
	if err != nil {
		return err
	}
	reportPlacement(a)

	// 2. Emit carved roots.
	outDir := f.out
	if outDir == "" {
		outDir = filepath.Join(srcDir, ".demono", "modules")
	}
	e := &emit.Emitter{SrcDir: srcDir, OutDir: outDir, Graph: a.Graph, Place: a.Placement, Bound: a.Boundary}
	ems, err := e.Emit()
	if err != nil {
		return err
	}
	moduleDirs := map[string]string{}
	outln("\nEmitted roots:")
	for _, em := range ems {
		moduleDirs[em.Module] = em.Dir
		outf("  %-16s %s (%d files)\n", em.Module, displayPath(srcDir, em.Dir), len(em.Files))
	}

	if !f.withState && !f.withProof && !f.withTfvars {
		outln("\nCode emitted. Re-run with --state to carve state, --tfvars to populate inputs, --proof to validate.")
		return nil
	}

	// 3. Carve state into per-module local files.
	stateWork := filepath.Join(outDir, ".state")
	plan := statemove.BuildPlan(a.Placement)
	opts := statemove.Options{
		ExecPath:        f.execPath,
		Engine:          statemove.Engine(f.engine),
		SourceStatePath: f.statePath,
	}
	carve, err := statemove.Carve(ctx, srcDir, stateWork, plan, opts)
	if err != nil {
		return fmt.Errorf("state carve: %w", err)
	}
	outln("\nCarved state (local copies, nothing pushed):")
	for _, em := range ems {
		if p, ok := carve.ModuleStates[em.Module]; ok {
			outf("  %-16s %s\n", em.Module, displayPath(srcDir, p))
		}
	}
	outf("  backup: %s\n", carve.BackupPath)

	// 4. Materialize per-module .tfvars from the applied source state, so each
	//    carved module is self-contained and provable standalone.
	if f.withTfvars || f.withProof {
		sourceState := f.statePath
		if sourceState == "" {
			// The carve's backup is the pre-mutation source state snapshot.
			sourceState = carve.BackupPath
		}
		st, err := statevars.LoadState(sourceState)
		if err != nil {
			return fmt.Errorf("tfvars: %w", err)
		}
		sv, err := statevars.Generate(st, a.Boundary, moduleDirs, statevars.Options{SourceDir: srcDir})
		if err != nil {
			return fmt.Errorf("tfvars: %w", err)
		}
		if len(sv.Files) > 0 {
			outln("\nGenerated input values (from applied state):")
			for _, em := range ems {
				if p, ok := sv.Files[em.Module]; ok {
					outf("  %-16s %s\n", em.Module, displayPath(srcDir, p))
				}
			}
		}
	}

	if !f.withProof {
		return nil
	}

	// 4. Graph-threaded zero-diff proof.
	pres, err := proof.Run(ctx, moduleDirs, carve.ModuleStates, a.Boundary, proof.Options{
		ExecPath: resolveExec(f),
		Refresh:  f.refresh,
	})
	if err != nil {
		return fmt.Errorf("proof: %w", err)
	}
	reportProof(pres)
	if !pres.OK {
		return fmt.Errorf("proof failed: at least one module plans changes against carved state")
	}
	return nil
}

func resolveExec(f splitFlags) string {
	if f.execPath != "" {
		return f.execPath
	}
	// Let tfexec resolve from PATH via the engine name.
	if p, err := lookEngine(f.engine); err == nil {
		return p
	}
	return f.engine
}

func reportPlacement(a *pipeline.Analysis) {
	outln("Placement:")
	for _, m := range a.Placement.ModuleNames() {
		outf("  %-16s %d resources/data\n", m, len(a.Placement.Modules[m]))
	}
	if len(a.Placement.Catchall) > 0 {
		outf("\nCatchall (%s) holds %d unannotated block(s):\n", a.Placement.Remainder, len(a.Placement.Catchall))
		for _, addr := range a.Placement.Catchall {
			outf("  %s\n", addr)
		}
	}
	if edges := a.Boundary.OrderingEdges; len(edges) > 0 {
		outln("\nCross-module depends_on (ordering must be enforced by the control plane):")
		for _, oe := range edges {
			outf("  %s → %s (%s depends_on %s)\n", oe.ConsumerModule, oe.ProducerModule, oe.Consumer, oe.Producer)
		}
	}
}

func reportProof(res *proof.Result) {
	outln("\nProof (per-module plan against carved state, inputs threaded from upstream outputs):")
	for _, m := range res.Order {
		mp := res.Modules[m]
		status := "zero-diff"
		if !mp.ZeroDiff {
			status = fmt.Sprintf("CHANGES +%d -%d", mp.AddCount, mp.Destroy)
		}
		outf("  %-16s %s (~%d in-place)\n", m, status, mp.Change)
	}
	if res.OK {
		outf("\n✓ %d modules, each plans to zero create/destroy with real threaded inputs.\n", len(res.Order))
	}
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

// lookEngine resolves the engine name (terraform/tofu) to a binary path.
func lookEngine(name string) (string, error) {
	if name == "" {
		name = "terraform"
	}
	return exec.LookPath(name)
}
