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
	"github.com/schrieksoft/demonolith/internal/statemove"
)

type migrateFlags struct {
	rootDir     string
	engine      string
	execPath    string
	file        string
	stateFile   string
	dryRun      bool
	output      string
	interactive bool
}

func migrateCmd() *cobra.Command {
	var f migrateFlags
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Execute the manifest's state moves against local state copies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrate(cmd.Context(), f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.engine, "engine", "", "state engine: terraform or tofu (required unless --dry-run)")
	flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	flags.StringVar(&f.file, "file", "", "execute exactly this manifest instead of all, in date order")
	flags.StringVar(&f.stateFile, "state-file", "", "carve this state snapshot instead of pulling from the configured backend")
	flags.BoolVar(&f.dryRun, "dry-run", false, "print the resolved state-move operation list without touching anything")
	flags.StringVar(&f.output, "output", "text", "report format: text or json")
	flags.BoolVarP(&f.interactive, "interactive", "i", false, "guided walkthrough: select manifests, preview the plan, confirm before executing")
	return cmd
}

// migrateManifestReport records one manifest's execution (or dry-run) result.
type migrateManifestReport struct {
	Manifest     string            `json:"manifest"`
	Skipped      bool              `json:"skipped"`
	SkipReason   string            `json:"skip_reason,omitempty"`
	DryRun       bool              `json:"dry_run,omitempty"`
	Moves        []moveReport      `json:"moves,omitempty"`
	ModuleStates map[string]string `json:"module_states,omitempty"`
	BackupPath   string            `json:"backup_path,omitempty"`
	ReceiptPath  string            `json:"receipt_path,omitempty"`
}

type moveReport struct {
	Address string `json:"address"`
	Module  string `json:"module"`
	Outcome string `json:"outcome"` // planned | moved | skipped
}

func runMigrate(ctx context.Context, f migrateFlags) error {
	if ctx == nil {
		ctx = context.Background()
	}
	mode, err := parseOutput(f.output)
	if err != nil {
		return err
	}
	if f.interactive {
		if mode == outputJSON {
			return fmt.Errorf("--interactive and --output json are mutually exclusive")
		}
		if !stdinIsTTY() {
			return fmt.Errorf("--interactive requires a terminal")
		}
	}
	rootDir := resolveRoot(f.rootDir)

	paths, err := selectManifests(rootDir, f.file)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no demonolith-refactor-*.yaml manifest found in %s; run `demonolith refactor` first", rootDir)
	}

	// The engine is only needed when moves actually execute.
	execPath := ""
	if !f.dryRun {
		if f.interactive && f.engine == "" && f.execPath == "" {
			engine, err := promptEngine()
			if err != nil {
				return err
			}
			f.engine = engine
		}
		execPath, err = engineExecPath(f.engine, f.execPath)
		if err != nil {
			return err
		}
	}

	if f.interactive {
		ok, err := confirmMigrate(rootDir, paths, f)
		if err != nil {
			return err
		}
		if !ok {
			outln("Aborted; nothing executed.")
			return nil
		}
	}

	var reports []migrateManifestReport
	var verdict error
	for _, path := range paths {
		rep, err := migrateOne(ctx, rootDir, path, execPath, f)
		if err != nil {
			if ExitCode(err) == ExitVerdict {
				reports = append(reports, migrateManifestReport{Manifest: filepath.Base(path), Skipped: true, SkipReason: err.Error()})
				verdict = err
				break
			}
			return err
		}
		reports = append(reports, *rep)
	}

	if mode == outputJSON {
		if err := printJSON(reports); err != nil {
			return err
		}
		return verdict
	}
	for _, rep := range reports {
		printMigrateReport(rootDir, rep)
	}
	return verdict
}

// selectManifests resolves --file or discovers all manifests in date order.
func selectManifests(rootDir, file string) ([]string, error) {
	if file == "" {
		return manifest.Discover(rootDir)
	}
	candidates := []string{file, filepath.Join(rootDir, file)}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return []string{c}, nil
		}
	}
	return nil, fmt.Errorf("manifest %s not found", file)
}

// migrateOne executes (or dry-runs) a single manifest.
func migrateOne(ctx context.Context, rootDir, path, execPath string, f migrateFlags) (*migrateManifestReport, error) {
	name := filepath.Base(path)
	rep := &migrateManifestReport{Manifest: name, DryRun: f.dryRun}

	m, err := manifest.Load(path)
	if err != nil {
		return nil, err
	}

	// Idempotency: a manifest whose receipt records a complete execution is
	// skipped, so re-running migrate resumes rather than erroring. Checked
	// before staleness so already-applied manifests from earlier refactors
	// don't trip the guard.
	receipt, err := manifest.LatestReceiptFor(rootDir, name)
	if err != nil {
		return nil, err
	}
	if receipt != nil && receipt.Complete {
		rep.Skipped = true
		rep.SkipReason = "already applied (complete receipt found)"
		return rep, nil
	}

	// Staleness: the emitted roots must be exactly what the manifest describes.
	moduleDirs := m.ModuleDirs(rootDir)
	sum, err := manifest.Checksum(moduleDirs)
	if err != nil {
		return nil, verdictf("manifest %s: emitted roots unreadable: %v", name, err)
	}
	if sum != m.EmitChecksum {
		return nil, verdictf("manifest %s is stale: the emitted roots changed after it was written; re-run `demonolith refactor`", name)
	}

	plan := m.Plan()
	if f.dryRun {
		workDir := filepath.Join(m.OutDir(rootDir), ".state")
		for _, mv := range m.StateMoves {
			rep.Moves = append(rep.Moves, moveReport{Address: mv.Address, Module: mv.Module, Outcome: "planned"})
		}
		rep.BackupPath = filepath.Join(workDir, "monolith.demono-backup.tfstate")
		return rep, nil
	}

	workDir := filepath.Join(m.OutDir(rootDir), ".state")
	opts := statemove.Options{
		ExecPath:        execPath,
		Engine:          statemove.Engine(f.engine),
		SourceStatePath: f.stateFile,
	}
	prep, err := statemove.Prepare(ctx, rootDir, workDir, opts)
	if err != nil {
		return nil, err
	}

	// Resume: filter out moves already applied in a prior partial run, and
	// refuse a manifest whose moves match neither the working state nor a
	// carved module state.
	filtered, outcomes, err := filterApplied(prep, workDir, plan, name)
	if err != nil {
		return nil, err
	}

	res, err := statemove.Execute(ctx, rootDir, workDir, prep, filtered, opts)
	if err != nil {
		return nil, fmt.Errorf("state carve: %w", err)
	}
	// A resumed run may have executed no moves for a module whose state was
	// carved earlier; the receipt must still record every carved state file.
	for mod := range plan.Moves {
		if _, ok := res.ModuleStates[mod]; ok {
			continue
		}
		st := filepath.Join(workDir, mod+".tfstate")
		if _, err := os.Stat(st); err == nil {
			res.ModuleStates[mod] = st
		}
	}

	for _, mv := range m.StateMoves {
		out := "moved"
		if outcomes[mv.Address] == "skipped" {
			out = "skipped"
		}
		rep.Moves = append(rep.Moves, moveReport{Address: mv.Address, Module: mv.Module, Outcome: out})
	}
	rep.ModuleStates = res.ModuleStates
	rep.BackupPath = res.BackupPath

	now := time.Now()
	r := &manifest.Receipt{
		Version:      manifest.SchemaVersion,
		Created:      now.UTC().Format(time.RFC3339),
		Tool:         toolString(),
		Manifest:     name,
		Engine:       f.engine,
		Complete:     true,
		ModuleStates: map[string]string{},
		BackupPath:   relForReceipt(rootDir, res.BackupPath),
	}
	for mod, st := range res.ModuleStates {
		r.ModuleStates[mod] = relForReceipt(rootDir, st)
	}
	for _, mv := range rep.Moves {
		r.Moves = append(r.Moves, manifest.MoveOutcome(mv))
	}
	receiptPath, err := manifest.WriteReceipt(r, rootDir, now)
	if err != nil {
		return nil, err
	}
	rep.ReceiptPath = receiptPath
	return rep, nil
}

// filterApplied drops moves a prior partial run already executed. A move whose
// source is absent from the working state and absent from its destination
// module state means the manifest does not match the state at all — refused.
func filterApplied(prep *statemove.Prepared, workDir string, plan *statemove.Plan, name string) (*statemove.Plan, map[string]string, error) {
	outcomes := map[string]string{}
	if !prep.Resumed {
		return plan, outcomes, nil
	}
	present, err := statemove.StateAddresses(prep.MonolithState)
	if err != nil {
		return nil, nil, err
	}
	filtered := &statemove.Plan{Moves: map[string][]statemove.Move{}, Remainder: plan.Remainder, AdoptRemainder: plan.AdoptRemainder}
	modules := make([]string, 0, len(plan.Moves))
	for mod := range plan.Moves {
		modules = append(modules, mod)
	}
	sort.Strings(modules)
	for _, mod := range modules {
		destAddrs := map[string]bool{}
		if mod != plan.Remainder {
			destAddrs, err = statemove.StateAddresses(filepath.Join(workDir, mod+".tfstate"))
			if err != nil {
				return nil, nil, err
			}
		}
		for _, mv := range plan.Moves[mod] {
			switch {
			case present[mv.SourceAddr]:
				filtered.Moves[mod] = append(filtered.Moves[mod], mv)
			case destAddrs[mv.DestAddr]:
				outcomes[mv.SourceAddr] = "skipped"
			default:
				return nil, nil, verdictf("manifest %s does not match the state: %s is in neither the monolith state nor module %s's state", name, mv.SourceAddr, mod)
			}
		}
	}
	return filtered, outcomes, nil
}

// relForReceipt stores receipt paths relative to rootDir when possible.
func relForReceipt(rootDir, p string) string {
	rel, err := filepath.Rel(rootDir, p)
	if err != nil || len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
		return p
	}
	return rel
}

func printMigrateReport(rootDir string, rep migrateManifestReport) {
	if rep.Skipped {
		outf("%s: skipped — %s\n", rep.Manifest, rep.SkipReason)
		return
	}
	if rep.DryRun {
		outf("%s: dry run — %d move(s):\n", rep.Manifest, len(rep.Moves))
		for _, mv := range rep.Moves {
			outf("  state mv %-40s -> %s\n", mv.Address, mv.Module)
		}
		outf("  backup would be written to %s\n", displayPath(rootDir, rep.BackupPath))
		return
	}
	outf("%s: applied\n", rep.Manifest)
	for _, mv := range rep.Moves {
		outf("  %-8s %-40s -> %s\n", mv.Outcome, mv.Address, mv.Module)
	}
	outln("\nCarved state (local copies, nothing pushed):")
	mods := make([]string, 0, len(rep.ModuleStates))
	for m := range rep.ModuleStates {
		mods = append(mods, m)
	}
	sort.Strings(mods)
	for _, m := range mods {
		outf("  %-16s %s\n", m, displayPath(rootDir, rep.ModuleStates[m]))
	}
	outf("  backup:  %s\n", displayPath(rootDir, rep.BackupPath))
	outf("  receipt: %s\n", displayPath(rootDir, rep.ReceiptPath))
}
