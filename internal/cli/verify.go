package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/schrieksoft/demonolith/internal/manifest"
	"github.com/schrieksoft/demonolith/internal/pipeline"
	"github.com/schrieksoft/demonolith/internal/proof"
	"github.com/schrieksoft/demonolith/internal/statemove"
	"github.com/schrieksoft/demonolith/internal/statevars"
)

type verifyFlags struct {
	rootDir      string
	engine       string
	execPath     string
	file         string
	stateFile    string
	refresh      bool
	keepTfvars   bool
	noTfvarsFile bool
	varFiles     []string
	vars         []string
	output       string
}

func verifyCmd() *cobra.Command {
	var f verifyFlags
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Prove the split is inert: per-module zero-diff plans with threaded inputs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(cmd.Context(), f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.engine, "engine", "", "state engine: terraform or tofu (required)")
	flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	flags.StringVar(&f.file, "file", "", "verify exactly this manifest (default: the newest)")
	flags.StringVar(&f.stateFile, "state-file", "", "source state snapshot for the ephemeral carve (default: pull from backend)")
	flags.BoolVar(&f.refresh, "refresh", false, "refresh state during the proof (authoritative but needs credentials)")
	flags.BoolVar(&f.keepTfvars, "keep-tfvars", false, "keep the generated.auto.tfvars files after the proof (they are the wiring for detached roots)")
	flags.BoolVar(&f.noTfvarsFile, "no-tfvars-file", false, "never write tfvars files; thread every value in memory only")
	flags.StringArrayVar(&f.varFiles, "var-file", nil, "additional tfvars file for external inputs (repeatable; overrides the root's auto-loaded files)")
	flags.StringArrayVar(&f.vars, "var", nil, "external input value as name=value (repeatable; overrides tfvars files and TF_VAR_*)")
	flags.StringVar(&f.output, "output", "text", "report format: text or json")
	return cmd
}

// verifyReport is the machine-facing proof result. External input values are
// deliberately absent — names only.
type verifyReport struct {
	Manifest       string                   `json:"manifest"`
	Mode           string                   `json:"mode"` // post-migrate | ephemeral
	OK             bool                     `json:"ok"`
	Order          []string                 `json:"order"`
	Modules        []manifest.ModuleVerdict `json:"modules"`
	ExternalInputs []string                 `json:"external_inputs,omitempty"`
	TfvarsFiles    map[string]string        `json:"tfvars_files,omitempty"`
	VerdictPath    string                   `json:"verdict_path"`
}

func runVerify(ctx context.Context, f verifyFlags) error {
	if ctx == nil {
		ctx = context.Background()
	}
	mode, err := parseOutput(f.output)
	if err != nil {
		return err
	}
	if f.keepTfvars && f.noTfvarsFile {
		return fmt.Errorf("--keep-tfvars and --no-tfvars-file are mutually exclusive")
	}
	rootDir := resolveRoot(f.rootDir)
	execPath, err := engineExecPath(f.engine, f.execPath)
	if err != nil {
		return err
	}

	m, name, err := loadNewestOrGiven(rootDir, f.file)
	if err != nil {
		return err
	}

	// Staleness: the roots being proven must be the ones the manifest describes.
	moduleDirs := m.ModuleDirs(rootDir)
	sum, err := manifest.Checksum(moduleDirs)
	if err != nil {
		return verdictf("manifest %s: emitted roots unreadable: %v", name, err)
	}
	if sum != m.EmitChecksum {
		return verdictf("manifest %s is stale: the emitted roots changed after it was written; re-run `demonolith refactor`", name)
	}

	// The proof and tfvars extraction need the boundary, which the manifest does
	// not fully carry; re-analyze and require the source to still match.
	a, err := pipeline.Analyze(rootDir, pipeline.Options{Remainder: m.Source.RemainderModule})
	if err != nil {
		return err
	}
	if !manifest.SemanticEqual(manifest.FromAnalysis(a), m) {
		return verdictf("manifest %s does not match the source analysis; re-run `demonolith refactor`", name)
	}

	// State sourcing: post-migrate when a receipt with intact carved states
	// exists; otherwise an ephemeral throwaway carve.
	moduleStates, sourceState, srcMode, cleanup, err := verifyStates(ctx, rootDir, name, m, execPath, f)
	if err != nil {
		return err
	}
	defer cleanup()

	extVals, extNames, err := collectExternalInputs(rootDir, a.Boundary, f.varFiles, f.vars)
	if err != nil {
		return err
	}

	rep := verifyReport{Manifest: name, Mode: srcMode, ExternalInputs: extNames}

	if !f.noTfvarsFile {
		st, err := statevars.LoadState(sourceState)
		if err != nil {
			return fmt.Errorf("tfvars: %w", err)
		}
		sv, err := statevars.Generate(st, a.Boundary, moduleDirs, statevars.Options{SourceDir: rootDir})
		if err != nil {
			return fmt.Errorf("tfvars: %w", err)
		}
		rep.TfvarsFiles = sv.Files
		if !f.keepTfvars {
			defer func() {
				for _, p := range sv.Files {
					_ = os.Remove(p)
				}
			}()
		}
	}

	pres, err := proof.Run(ctx, moduleDirs, moduleStates, a.Boundary, proof.Options{
		ExecPath:       execPath,
		Refresh:        f.refresh,
		ExternalInputs: extVals,
	})
	if err != nil {
		return fmt.Errorf("proof: %w", err)
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
		Version:        manifest.SchemaVersion,
		Created:        now.UTC().Format(time.RFC3339),
		Tool:           toolString(),
		Manifest:       name,
		Refresh:        f.refresh,
		OK:             pres.OK,
		Order:          pres.Order,
		Modules:        rep.Modules,
		ExternalInputs: extNames,
	}
	vpath, err := manifest.WriteVerdict(verdict, rootDir, now)
	if err != nil {
		return err
	}
	rep.VerdictPath = vpath

	if mode == outputJSON {
		if err := printJSON(rep); err != nil {
			return err
		}
	} else {
		printVerifyReport(rootDir, rep)
	}
	if !pres.OK {
		return verdictf("proof failed: at least one module plans a create or destroy against carved state")
	}
	return nil
}

// loadNewestOrGiven loads --file, or the newest manifest in rootDir.
func loadNewestOrGiven(rootDir, file string) (*manifest.Manifest, string, error) {
	paths, err := selectManifests(rootDir, file)
	if err != nil {
		return nil, "", err
	}
	if len(paths) == 0 {
		return nil, "", fmt.Errorf("no demonolith-refactor-*.yaml manifest found in %s; run `demonolith refactor` first", rootDir)
	}
	path := paths[len(paths)-1]
	m, err := manifest.Load(path)
	if err != nil {
		return nil, "", err
	}
	return m, filepath.Base(path), nil
}

// verifyStates decides where the carved states come from. Post-migrate mode
// proves the actual migration output; ephemeral mode carves a throwaway local
// copy so a proof can run before any real migration (safe by construction —
// carving is local-only). The returned source state is the pre-carve applied
// snapshot tfvars extraction reads.
func verifyStates(ctx context.Context, rootDir, name string, m *manifest.Manifest, execPath string, f verifyFlags) (map[string]string, string, string, func(), error) {
	noop := func() {}

	receipt, err := manifest.LatestReceiptFor(rootDir, name)
	if err != nil {
		return nil, "", "", noop, err
	}
	if receipt != nil && receipt.Complete {
		states := map[string]string{}
		intact := true
		for mod, p := range receipt.ModuleStates {
			rp := manifest.Resolve(rootDir, p)
			if _, err := os.Stat(rp); err != nil {
				intact = false
				break
			}
			states[mod] = rp
		}
		backup := manifest.Resolve(rootDir, receipt.BackupPath)
		if _, err := os.Stat(backup); err != nil {
			intact = false
		}
		if intact {
			return states, backup, "post-migrate", noop, nil
		}
	}

	tmp, err := os.MkdirTemp("", "demono-verify-*")
	if err != nil {
		return nil, "", "", noop, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	opts := statemove.Options{
		ExecPath:        execPath,
		Engine:          statemove.Engine(f.engine),
		SourceStatePath: f.stateFile,
	}
	carve, err := statemove.Carve(ctx, rootDir, tmp, m.Plan(), opts)
	if err != nil {
		cleanup()
		return nil, "", "", noop, fmt.Errorf("ephemeral carve: %w", err)
	}
	return carve.ModuleStates, carve.BackupPath, "ephemeral", cleanup, nil
}

func printVerifyReport(rootDir string, rep verifyReport) {
	outf("Proof (%s mode; per-module plan against carved state, inputs threaded from upstream outputs):\n", rep.Mode)
	for _, mv := range rep.Modules {
		status := "zero-diff"
		if !mv.ZeroDiff {
			status = fmt.Sprintf("CHANGES +%d -%d", mv.Create, mv.Destroy)
		}
		outf("  %-16s %s (~%d in-place)\n", mv.Module, status, mv.Update)
	}
	if len(rep.TfvarsFiles) > 0 {
		outln("\nGenerated input values (from applied state):")
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
		outf("\n✓ %d modules, each plans to zero create/destroy with real threaded inputs.\n", len(rep.Order))
	}
	outf("Verdict: %s\n", displayPath(rootDir, rep.VerdictPath))
}
