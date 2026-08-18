package cli

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/schrieksoft/demonolith/internal/manifest"
	"github.com/schrieksoft/demonolith/internal/proof"
)

func migrateProveCmd() *cobra.Command {
	var f migrateFlags
	cmd := &cobra.Command{
		Use:   "prove",
		Short: "Prove migrate map's output inert: threaded zero-diff plans over the carved state copies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrateProve(cmd.Context(), f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.engine, "engine", "", "state engine: terraform or tofu (required)")
	flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	flags.BoolVar(&f.refresh, "refresh", false, "refresh state during the proof (authoritative but needs credentials)")
	flags.BoolVar(&f.noTfvars, "no-tfvars", false, "do not materialize demono.root.tfvars/demono.graph.tfvars; thread all values in memory only (for tests)")
	flags.StringArrayVar(&f.varFiles, "var-file", nil, "additional tfvars file for external inputs (repeatable; overrides the root's auto-loaded files)")
	flags.StringArrayVar(&f.vars, "var", nil, "external input value as name=value (repeatable; overrides tfvars files and TF_VAR_*)")
	flags.StringVar(&f.output, "output", "text", "report format: text or json")
	return cmd
}

// proveReport is the machine-facing proof result. External input values are
// deliberately absent — names only.
type proveReport struct {
	Manifest       string                   `json:"manifest"`
	Mode           string                   `json:"mode"`
	OK             bool                     `json:"ok"`
	Order          []string                 `json:"order"`
	Modules        []manifest.ModuleVerdict `json:"modules"`
	ExternalInputs []string                 `json:"external_inputs,omitempty"`
	TfvarsFiles    map[string]string        `json:"tfvars_files,omitempty"`
	VerdictPath    string                   `json:"verdict_path"`
}

func runMigrateProve(ctx context.Context, f migrateFlags) error {
	if ctx == nil {
		ctx = context.Background()
	}
	mode, err := parseOutput(f.output)
	if err != nil {
		return err
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
		Refresh:        f.refresh,
		ExternalInputs: extVals,
		RootInputs:     rootInputs,
	}
	if mode == outputText {
		outln("Proving modules in dependency order (plans against the carved state copies):")
		opts.OnPlanStart = func(module string) { outf("  %s: planning ... ", module) }
		opts.OnPlanDone = func(_, verdict string) { outf("%s\n", verdict) }
	}
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
		Refresh:          f.refresh,
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

	if mode == outputJSON {
		if err := printJSON(rep); err != nil {
			return err
		}
	} else {
		printProofReport(rootDir, rep)
	}
	if !pres.OK {
		return verdictf("proof failed: at least one module plans changes against carved state")
	}
	return nil
}

// printProofReport prints what the live per-module progress lines did not
// already say: materialized files, unresolved inputs, the total, the verdict.
func printProofReport(rootDir string, rep proveReport) {
	if len(rep.TfvarsFiles) > 0 {
		outln("\nMaterialized root variable values (demono.root.tfvars):")
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
		outf("\n✓ %d modules, each plans to zero changes with real threaded inputs.\n", len(rep.Order))
	}
	outf("Verdict: %s\n", displayPath(rootDir, rep.VerdictPath))
}
