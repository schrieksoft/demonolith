package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/schrieksoft/demonolith/internal/manifest"
	"github.com/schrieksoft/demonolith/internal/proof"
)

func migrateVerifyCmd() *cobra.Command {
	var f migrateFlags
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Judge the executed migration: plan each root against its real backend, assert zero create/destroy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrateVerify(cmd.Context(), f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.engine, "engine", "", "state engine: terraform or tofu (required)")
	flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	flags.StringArrayVar(&f.backendConfig, "backend-config", nil, "out-of-band backend config value for init, as key=value (repeatable)")
	flags.StringArrayVar(&f.varFiles, "var-file", nil, "additional tfvars file for external inputs (repeatable)")
	flags.StringArrayVar(&f.vars, "var", nil, "external input value as name=value (repeatable)")
	flags.StringVar(&f.output, "output", "text", "report format: text or json")
	return cmd
}

// runMigrateVerify is the post-run judgment: the threaded proof executed
// against each root's real backend — no staged state copies, a full init.
// Requires the migration to have been run (a complete run receipt).
func runMigrateVerify(ctx context.Context, f migrateFlags) error {
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
	runReceipt, err := manifest.LatestReceiptFor(rootDir, m.EmitChecksum, manifest.ActionRun)
	if err != nil {
		return err
	}
	if runReceipt == nil || !runReceipt.Complete {
		return fmt.Errorf("no completed migrate run for this manifest generation; run `demonolith migrate run` first")
	}
	a, err := analyzeMatching(rootDir, m)
	if err != nil {
		return err
	}

	if err := materializeBackendEnv(rootDir, m); err != nil {
		return err
	}

	extVals, extNames, err := collectExternalInputs(rootDir, a.Boundary, f.varFiles, f.vars)
	if err != nil {
		return err
	}

	pres, err := proof.Run(ctx, m.ModuleDirs(rootDir), nil, a.Boundary, proof.Options{
		ExecPath:       execPath,
		Refresh:        true,
		ExternalInputs: extVals,
		UseBackend:     true,
		BackendConfig:  f.backendConfig,
	})
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}

	rep := proveReport{Manifest: manifest.FileName, Mode: manifest.ModeFinal, ExternalInputs: extNames}
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
		Mode:             manifest.ModeFinal,
		Refresh:          true,
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
		printProofReport(rootDir, rep, "per-module plan against the real backends")
	}
	if !pres.OK {
		return verdictf("verification failed: at least one module plans a create or destroy against its real backend")
	}
	return nil
}
