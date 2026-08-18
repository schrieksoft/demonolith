package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	output      string
	interactive bool
	monorepo    bool
	noBootstrap bool
	noBackend   bool
	quiet       bool
	silent      bool
	validate    bool
	engine      string
	execPath    string
	yes         bool
}

func refactorCmd() *cobra.Command {
	var f refactorFlags
	cmd := &cobra.Command{
		Use:   "refactor",
		Short: "Carve the monolith's code: map → run → verify",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRefactorPipeline(f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.out, "out", "", "output directory for carved roots, resolved against --root-dir and required to be inside it (default `modules`)")
	flags.StringVar(&f.remainder, "remainder-module", "legacy", "catchall module name for unannotated blocks")
	flags.BoolVar(&f.monorepo, "monorepo", false, "link in-repo child modules by relative path instead of copying them")
	flags.BoolVar(&f.noBootstrap, "no-bootstrap", false, "skip the Snap CD bootstrap module")
	flags.BoolVar(&f.noBackend, "no-backend", false, "skip backend derivation: carve roots without backend blocks")
	flags.StringVar(&f.output, "output", "text", "report format: text or json")
	flags.BoolVarP(&f.interactive, "interactive", "i", false, "guided walkthrough of the whole pipeline")
	flags.BoolVarP(&f.yes, "yes", "y", false, "approve the plan automatically instead of pausing for confirmation before run")

	cmd.AddCommand(refactorMapCmd(), refactorRunCmd(), refactorVerifyCmd())
	return cmd
}

func refactorMapCmd() *cobra.Command {
	var f refactorFlags
	cmd := &cobra.Command{
		Use:   "map",
		Short: "Analyze the monolith and write the map of the split — the reviewable plan; nothing is emitted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := parseOutput(f.output)
			if err != nil {
				return err
			}
			if f.interactive {
				if mode == outputJSON {
					return fmt.Errorf("--interactive and --output json are mutually exclusive")
				}
				return runRefactorMapInteractive(f)
			}
			_, err = runRefactorMap(f, mode)
			return err
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.out, "out", "", "output directory for carved roots, resolved against --root-dir and required to be inside it (default `modules`)")
	flags.StringVar(&f.remainder, "remainder-module", "legacy", "catchall module name for unannotated blocks")
	flags.BoolVar(&f.monorepo, "monorepo", false, "link in-repo child modules by relative path instead of copying them")
	flags.BoolVar(&f.noBootstrap, "no-bootstrap", false, "skip the Snap CD bootstrap module")
	flags.BoolVar(&f.noBackend, "no-backend", false, "skip backend derivation: carve roots without backend blocks")
	flags.StringVar(&f.output, "output", "text", "report format: text or json")
	flags.BoolVarP(&f.interactive, "interactive", "i", false, "guided walkthrough: prompt for parameters, triage the catchall, confirm before writing the manifest")
	return cmd
}

func refactorRunCmd() *cobra.Command {
	var f refactorFlags
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Execute the map: emit carved roots, backends, and bootstrap; finalize the checksum",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := parseOutput(f.output)
			if err != nil {
				return err
			}
			return runRefactorRun(resolveRoot(f.rootDir), mode)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.output, "output", "text", "report format: text or json")
	return cmd
}

// planReport is the machine-facing result of refactor map.
type planReport struct {
	Modules        map[string]int    `json:"modules"`
	Catchall       []string          `json:"catchall,omitempty"`
	OrderingEdges  []string          `json:"ordering_edges,omitempty"`
	PlannedDirs    map[string]string `json:"planned_dirs"`
	BackendType    string            `json:"backend_type,omitempty"`
	StateLocations map[string]string `json:"state_locations,omitempty"`
	Bootstrap      bool              `json:"bootstrap"`
	ManifestPath   string            `json:"map_path"`
}

// runRefactorMap analyzes and writes the planned manifest. Nothing is emitted;
// run's pre-flights (target-dir collisions, backend-type support, the reserved
// bootstrap name) are enforced here so a written plan is a runnable plan.
func runRefactorMap(f refactorFlags, mode outputMode) (*manifest.Manifest, error) {
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

	// Pre-flight against the manifest currently on disk (the previous
	// generation's ownership record) before overwriting it.
	if err := checkTargetDirs(a, rootDir, outDir, opts.Bootstrap); err != nil {
		return nil, err
	}

	m := manifest.BuildPlanned(a, rootDir, outDir, time.Now(), toolString(), opts)
	path := manifest.Path(rootDir)
	if err := manifest.Write(m, path); err != nil {
		return nil, err
	}

	if mode == outputJSON {
		rep := planReport{Modules: map[string]int{}, PlannedDirs: map[string]string{}, Bootstrap: opts.Bootstrap, ManifestPath: path}
		for _, name := range a.Placement.ModuleNames() {
			rep.Modules[name] = len(a.Placement.Modules[name])
			rep.PlannedDirs[name] = m.Modules[name].Dir
		}
		for _, addr := range a.Placement.Catchall {
			rep.Catchall = append(rep.Catchall, addr.String())
		}
		for _, oe := range a.Boundary.OrderingEdges {
			rep.OrderingEdges = append(rep.OrderingEdges, fmt.Sprintf("%s -> %s (%s depends_on %s)", oe.ConsumerModule, oe.ProducerModule, oe.Consumer, oe.Producer))
		}
		if m.Backend != nil {
			rep.BackendType = m.Backend.Type
			rep.StateLocations = m.Backend.Modules
		}
		return m, printJSON(rep)
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
	return m, nil
}

// runReport is the machine-facing result of refactor run.
type runReport struct {
	EmittedDirs  map[string]string `json:"emitted_dirs"`
	BootstrapDir string            `json:"bootstrap_dir,omitempty"`
	ManifestPath string            `json:"map_path"`
}

// runRefactorRun executes the planned manifest verbatim: emit the carved roots
// (and backends and bootstrap per the plan), then finalize the checksum. The
// source must still match the plan.
func runRefactorRun(rootDir string, mode outputMode) error {
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

	if mode == outputJSON {
		rep := runReport{EmittedDirs: map[string]string{}, ManifestPath: path, BootstrapDir: m.Output.BootstrapDir}
		for _, em := range ems {
			rep.EmittedDirs[em.Module] = em.Dir
		}
		return printJSON(rep)
	}
	outln(heading("Module directories written:"))
	for _, em := range ems {
		outf("  %-16s %s (%d files)\n", em.Module, displayPath(rootDir, em.Dir), len(em.Files))
	}
	if bsDir != "" {
		outf("  %-16s %s (Snap CD bootstrap)\n", bootstrap.DirName, displayPath(rootDir, bsDir))
	}
	outln("\n" + heading("Receipt:"))
	outf("  %s %s\n", displayPath(rootDir, path), dim("(finalized)"))
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

// runRefactorPipeline is the bare `demonolith refactor`: map → run → verify.
func runRefactorPipeline(f refactorFlags) error {
	mode, err := parseOutput(f.output)
	if err != nil {
		return err
	}
	if f.interactive {
		if mode == outputJSON {
			return fmt.Errorf("--interactive and --output json are mutually exclusive")
		}
		return runRefactorInteractivePipeline(f)
	}
	outf("\n%s\n\n", banner("── refactor map ──"))
	mf := f
	mf.interactive = true // suppress the standalone "review it, then run" hint
	if _, err := runRefactorMap(mf, mode); err != nil {
		return err
	}
	if !f.yes {
		if !stdinIsTTY() {
			return fmt.Errorf("the pipeline pauses for approval after plan; pass -y to approve automatically, or run the subcommands individually")
		}
		ok, err := promptYesNo("\nRun the refactor now?", false)
		if err != nil {
			return err
		}
		if !ok {
			outln("Map written; run later with `demonolith refactor run`.")
			return nil
		}
	}
	rootDir := resolveRoot(f.rootDir)
	outf("\n%s\n\n", banner("── refactor run ──"))
	if err := runRefactorRun(rootDir, mode); err != nil {
		return err
	}
	outf("\n%s\n\n", banner("── refactor verify ──"))
	vf := f
	vf.quiet = true
	return runRefactorVerify(rootDir, mode, vf)
}

// checkTargetDirs refuses, before the manifest is overwritten, any target
// module dir that already exists and is not demonolith's own previous output (a
// dir the manifest on disk records). Emitting into a foreign dir — typically an
// existing child module sharing a carved module's name — would merge generated
// output into content demonolith does not own. All collisions are reported at
// once, and nothing is written. This runs at plan time; run trusts the plan,
// accepting only dirs the manifest itself records.
func checkTargetDirs(a *pipeline.Analysis, rootDir, outDir string, withBootstrap bool) error {
	owned := map[string]bool{}
	if prev, err := manifest.Load(manifest.Path(rootDir)); err == nil {
		for _, d := range prev.ChecksumDirs(rootDir) {
			owned[filepath.Clean(d)] = true
		}
		// A planned-but-never-run previous manifest still owns its planned dirs.
		for _, d := range prev.ModuleDirs(rootDir) {
			owned[filepath.Clean(d)] = true
		}
	}
	targets := a.Placement.ModuleNames()
	if withBootstrap {
		for _, name := range targets {
			if name == bootstrap.DirName {
				return fmt.Errorf("module name %q is reserved for the Snap CD bootstrap module; rename the carved module or pass --no-bootstrap", bootstrap.DirName)
			}
		}
		targets = append(append([]string(nil), targets...), bootstrap.DirName)
	}
	var foreign []string
	for _, name := range targets {
		dir := filepath.Clean(filepath.Join(outDir, name))
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		// A dir without a single file in it holds nothing to protect.
		if !owned[dir] && dirHasFiles(dir) {
			foreign = append(foreign, displayPath(rootDir, dir))
		}
	}
	if len(foreign) > 0 {
		return fmt.Errorf("target module dir(s) already exist and are not demonolith output: %s — refusing to emit into content demonolith does not own; rename the carved module(s) or move the existing dir(s)", strings.Join(foreign, ", "))
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

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stdout, string(b))
	return nil
}
