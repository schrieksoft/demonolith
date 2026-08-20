package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/schrieksoft/demonolith/internal/hclgraph"
	"github.com/schrieksoft/demonolith/internal/manifest"
	"github.com/schrieksoft/demonolith/internal/placement"
	"github.com/schrieksoft/demonolith/internal/proof"
)

func migrateProveCmd() *cobra.Command {
	var f migrateFlags
	cmd := &cobra.Command{
		Use:   "prove",
		Short: "Prove the split changes nothing: plan every module against its local state copy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrateProve(cmd.Context(), f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.engine, "engine", "", "state engine: terraform or tofu (required)")
	flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	flags.BoolVar(&f.noTfvars, "no-tfvars", false, "do not write demono.root.tfvars/demono.graph.tfvars; pass all values in memory only (for tests)")
	flags.StringArrayVar(&f.varFiles, "var-file", nil, "additional tfvars file for external inputs (repeatable; overrides the root's auto-loaded files)")
	flags.StringArrayVar(&f.vars, "var", nil, "external input value as name=value (repeatable; overrides tfvars files and TF_VAR_*)")
	return cmd
}

// proveReport collects the proof result for reporting. External input values
// are deliberately absent — names only.
type proveReport struct {
	Manifest       string
	Mode           string
	OK             bool
	Order          []string
	Modules        []manifest.ModuleVerdict
	ExternalInputs []string
	TfvarsFiles    map[string]string
	VerdictPath    string
}

// printLiveReads lists, per module, the data sources its plan will read live —
// the one channel -refresh=false cannot freeze, and the only ambient
// credential need beyond provider configuration.
func printLiveReads(place *placement.Placement) {
	names := place.ModuleNames()
	rows := make([][2]string, 0, len(names))
	for _, mod := range names {
		var reads []string
		for _, addr := range place.Modules[mod] {
			if addr.Kind == hclgraph.KindData {
				reads = append(reads, addr.String())
			}
		}
		if len(reads) > 0 {
			sort.Strings(reads)
			rows = append(rows, [2]string{mod, strings.Join(reads, ", ")})
		}
	}
	if len(rows) == 0 {
		outln("No data sources: nothing is read live during the plans.")
		return
	}
	outln(heading("Live reads") + " (data sources are planned fresh; their answers must hold still):")
	for _, r := range rows {
		outf("  %-16s %s\n", r[0], r[1])
	}
}

func runMigrateProve(ctx context.Context, f migrateFlags) error {
	if ctx == nil {
		ctx = context.Background()
	}
	rootDir := resolveRoot(f.rootDir)
	execPath, err := engineExecPath(f.engine, f.execPath)
	if err != nil {
		return err
	}

	m, err := loadRunManifest(rootDir)
	if err != nil {
		return err
	}
	a, err := analyzeMatching(rootDir, m)
	if err != nil {
		return err
	}

	// The subject of the proof: migrate map's carved artifacts.
	_, moduleStates, _, err := mapReceiptStates(rootDir, m)
	if err != nil {
		return err
	}
	moduleDirs := m.ModuleDirs(rootDir)

	extVals, extNames, err := collectExternalInputs(rootDir, a.Boundary, f.varFiles, f.vars)
	if err != nil {
		return err
	}

	rep := proveReport{Manifest: manifest.FileName, Mode: manifest.ModeProve, ExternalInputs: extNames}

	sv, err := materializeRootTfvars(rootDir, m, a.Boundary, f)
	if err != nil {
		return err
	}
	rep.TfvarsFiles = sv.Files

	var rootInputs map[string]map[string]string
	if f.noTfvars {
		rootInputs = sv.Values
	}
	opts := proof.Options{
		ExecPath:       execPath,
		ExternalInputs: extVals,
		RootInputs:     rootInputs,
	}
	printLiveReads(a.Placement)
	outln("\n" + heading("Proving modules in dependency order") + " (plans against the local state copies):")
	opts.OnPlanStart = func(module string) { outf("  %s: proving ... ", module) }
	opts.OnPlanDone = func(_, verdict string) { outf("%s\n", colorVerdict(verdict)) }
	pres, err := proof.Run(ctx, moduleDirs, moduleStates, a.Boundary, opts)
	if err != nil {
		return fmt.Errorf("proof: %w", err)
	}
	if err := writeProofThreaded(rootDir, m, pres.Threaded); err != nil {
		return err
	}

	rep.OK = pres.OK
	rep.Order = pres.Order
	for _, mod := range pres.Order {
		mp := pres.Modules[mod]
		rep.Modules = append(rep.Modules, manifest.ModuleVerdict{
			Module: mod, ZeroDiff: mp.ZeroDiff, Create: mp.AddCount, Destroy: mp.Destroy, Update: mp.Change,
		})
	}

	now := time.Now()
	verdict := &manifest.Verdict{
		Version:          manifest.SchemaVersion,
		Created:          now.UTC().Format(time.RFC3339),
		Tool:             toolString(),
		Manifest:         manifest.FileName,
		ManifestChecksum: m.EmitChecksum,
		Mode:             manifest.ModeProve,
		Refresh:          false,
		OK:               pres.OK,
		Order:            pres.Order,
		Modules:          rep.Modules,
		ExternalInputs:   extNames,
	}
	vpath, err := manifest.WriteVerdict(verdict, rootDir)
	if err != nil {
		return err
	}
	rep.VerdictPath = vpath

	printProofReport(rootDir, rep)
	if !pres.OK {
		return verdictf("proof failed: at least one module's plan shows changes")
	}
	return nil
}

// printProofReport prints what the live per-module progress lines did not
// already say: materialized files, unresolved inputs, the total, the verdict.
func printProofReport(rootDir string, rep proveReport) {
	if len(rep.TfvarsFiles) > 0 {
		outln("\n" + heading("Root variable values written (demono.root.tfvars):"))
		mods := make([]string, 0, len(rep.TfvarsFiles))
		for m := range rep.TfvarsFiles {
			mods = append(mods, m)
		}
		sort.Strings(mods)
		for _, m := range mods {
			outf("  %-16s %s\n", m, displayPath(rootDir, rep.TfvarsFiles[m]))
		}
	}
	if rep.OK {
		outf("\n%s\n", success(fmt.Sprintf("✓ %d modules, each plans to zero changes with its real input values.", len(rep.Order))))
	}
	outln("\n" + heading("Receipt:"))
	outf("  %s\n\n", displayPath(rootDir, rep.VerdictPath))
}
