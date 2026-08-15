package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/manifest"
	"github.com/schrieksoft/demonolith/internal/pipeline"
)

type refactorFlags struct {
	rootDir     string
	out         string
	remainder   string
	check       bool
	output      string
	interactive bool
}

func refactorCmd() *cobra.Command {
	var f refactorFlags
	cmd := &cobra.Command{
		Use:   "refactor",
		Short: "Carve the monolith's code into per-module roots and write a manifest",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRefactor(f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.out, "out", "", "output directory for carved roots (default <root-dir>/.demono/modules)")
	flags.StringVar(&f.remainder, "remainder-module", "monolith", "catchall module name for unannotated blocks")
	flags.BoolVar(&f.check, "check", false, "drift gate: re-run analysis+emit in memory and compare against the committed roots and manifest; writes nothing")
	flags.StringVar(&f.output, "output", "text", "report format: text or json")
	flags.BoolVarP(&f.interactive, "interactive", "i", false, "guided walkthrough: triage the catchall, write assignments back as decorators, confirm before emitting")
	return cmd
}

// refactorReport is the machine-facing result of a refactor run.
type refactorReport struct {
	Modules       map[string]int `json:"modules"`
	Catchall      []string       `json:"catchall,omitempty"`
	OrderingEdges []string       `json:"ordering_edges,omitempty"`
	EmittedDirs   map[string]string `json:"emitted_dirs"`
	ManifestPath  string         `json:"manifest_path"`
}

// checkReport is the machine-facing result of refactor --check.
type checkReport struct {
	Drift        bool     `json:"drift"`
	Manifest     string   `json:"manifest,omitempty"`
	ChangedFiles []string `json:"changed_files,omitempty"`
	Reasons      []string `json:"reasons,omitempty"`
}

func runRefactor(f refactorFlags) error {
	mode, err := parseOutput(f.output)
	if err != nil {
		return err
	}
	if f.interactive && mode == outputJSON {
		return fmt.Errorf("--interactive and --output json are mutually exclusive")
	}
	rootDir := resolveRoot(f.rootDir)

	if f.check {
		if f.interactive {
			return fmt.Errorf("--check does not support --interactive")
		}
		return runRefactorCheck(rootDir, f, mode)
	}
	if f.interactive {
		return runRefactorInteractive(rootDir, f)
	}

	a, err := pipeline.Analyze(rootDir, pipeline.Options{Remainder: f.remainder})
	if err != nil {
		return err
	}
	outDir := f.out
	if outDir == "" {
		outDir = filepath.Join(rootDir, ".demono", "modules")
	}
	ems, m, path, err := emitAndWriteManifest(a, rootDir, outDir)
	if err != nil {
		return err
	}

	if mode == outputJSON {
		return printJSON(buildRefactorReport(a, ems, path))
	}
	reportAnalysis(a)
	outln("\nEmitted roots:")
	for _, em := range ems {
		outf("  %-16s %s (%d files)\n", em.Module, displayPath(rootDir, em.Dir), len(em.Files))
	}
	outf("\nManifest: %s (%d state moves, %d cross edges)\n", displayPath(rootDir, path), len(m.StateMoves), len(m.CrossEdges))
	return nil
}

// emitAndWriteManifest is the shared back half of a (non-check) refactor run.
func emitAndWriteManifest(a *pipeline.Analysis, rootDir, outDir string) ([]emit.EmittedModule, *manifest.Manifest, string, error) {
	e := &emit.Emitter{SrcDir: rootDir, OutDir: outDir, Graph: a.Graph, Place: a.Placement, Bound: a.Boundary}
	ems, err := e.Emit()
	if err != nil {
		return nil, nil, "", err
	}
	now := time.Now()
	m, err := manifest.Build(a, rootDir, outDir, ems, now, toolString())
	if err != nil {
		return nil, nil, "", err
	}
	path := filepath.Join(rootDir, manifest.FileName(now))
	if err := manifest.Write(m, path); err != nil {
		return nil, nil, "", err
	}
	return ems, m, path, nil
}

func buildRefactorReport(a *pipeline.Analysis, ems []emit.EmittedModule, manifestPath string) refactorReport {
	r := refactorReport{Modules: map[string]int{}, EmittedDirs: map[string]string{}, ManifestPath: manifestPath}
	for _, m := range a.Placement.ModuleNames() {
		r.Modules[m] = len(a.Placement.Modules[m])
	}
	for _, addr := range a.Placement.Catchall {
		r.Catchall = append(r.Catchall, addr.String())
	}
	for _, oe := range a.Boundary.OrderingEdges {
		r.OrderingEdges = append(r.OrderingEdges, fmt.Sprintf("%s -> %s (%s depends_on %s)", oe.ConsumerModule, oe.ProducerModule, oe.Consumer, oe.Producer))
	}
	for _, em := range ems {
		r.EmittedDirs[em.Module] = em.Dir
	}
	return r
}

// runRefactorCheck re-runs analysis+emit into a temp dir and compares against
// the committed roots and the newest committed manifest. Nothing is written to
// the root or output dirs; any mismatch is a negative verdict.
func runRefactorCheck(rootDir string, f refactorFlags, mode outputMode) error {
	a, err := pipeline.Analyze(rootDir, pipeline.Options{Remainder: f.remainder})
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "demono-check-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	e := &emit.Emitter{SrcDir: rootDir, OutDir: tmp, Graph: a.Graph, Place: a.Placement, Bound: a.Boundary}
	ems, err := e.Emit()
	if err != nil {
		return err
	}
	freshDirs := map[string]string{}
	for _, em := range ems {
		freshDirs[em.Module] = em.Dir
	}
	fresh, err := manifest.Build(a, rootDir, tmp, ems, time.Now(), toolString())
	if err != nil {
		return err
	}

	rep := checkReport{}
	paths, err := manifest.Discover(rootDir)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		rep.Drift = true
		rep.Reasons = append(rep.Reasons, "no committed manifest found; run `demonolith refactor` and commit its output")
		return reportCheck(rep, mode)
	}
	newest := paths[len(paths)-1]
	rep.Manifest = filepath.Base(newest)
	committed, err := manifest.Load(newest)
	if err != nil {
		return err
	}

	committedHashes, err := manifest.FileHashes(committed.ModuleDirs(rootDir))
	if err != nil {
		rep.Drift = true
		rep.Reasons = append(rep.Reasons, fmt.Sprintf("committed roots unreadable: %v", err))
		return reportCheck(rep, mode)
	}
	freshHashes, err := manifest.FileHashes(freshDirs)
	if err != nil {
		return err
	}

	rep.ChangedFiles = diffHashes(freshHashes, committedHashes)
	if len(rep.ChangedFiles) > 0 {
		rep.Drift = true
		rep.Reasons = append(rep.Reasons, "committed roots differ from what the committed source produces")
	}
	if committed.EmitChecksum != manifest.ChecksumOf(committedHashes) {
		rep.Drift = true
		rep.Reasons = append(rep.Reasons, "committed roots were edited after the manifest was written (emit_checksum mismatch)")
	}
	if !manifest.SemanticEqual(fresh, committed) {
		rep.Drift = true
		rep.Reasons = append(rep.Reasons, "manifest content differs from what the committed source produces")
	}
	return reportCheck(rep, mode)
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

func reportCheck(rep checkReport, mode outputMode) error {
	if mode == outputJSON {
		if err := printJSON(rep); err != nil {
			return err
		}
	} else if rep.Drift {
		outln("Drift detected:")
		for _, r := range rep.Reasons {
			outf("  - %s\n", r)
		}
		for _, f := range rep.ChangedFiles {
			outf("    %s\n", f)
		}
	} else {
		outf("No drift: committed roots and manifest %s match the committed source.\n", rep.Manifest)
	}
	if rep.Drift {
		return verdictf("drift detected; re-run `demonolith refactor` and commit its output")
	}
	return nil
}

// reportAnalysis prints the placement, catchall, and ordering-edge summary.
func reportAnalysis(a *pipeline.Analysis) {
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

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stdout, string(b))
	return nil
}
