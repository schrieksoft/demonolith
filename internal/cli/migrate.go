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
	"github.com/schrieksoft/demonolith/internal/statemove"
)

// migrateFlags is the union of the migrate subcommands' flags; the bare parent
// pipeline takes them all.
type migrateFlags struct {
	rootDir       string
	engine        string
	execPath      string
	stateFile     string
	output        string
	interactive   bool
	refresh       bool
	createTfvars  bool
	varFiles      []string
	vars          []string
	backendConfig []string
	unproven      bool
	yes           bool
}

func migrateCmd() *cobra.Command {
	var f migrateFlags
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate the state: plan → prove → run → verify",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigratePipeline(cmd.Context(), f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.engine, "engine", "", "state engine: terraform or tofu (required)")
	flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	flags.StringVar(&f.stateFile, "state-file", "", "carve this state snapshot instead of pulling from the configured backend")
	flags.StringArrayVar(&f.varFiles, "var-file", nil, "additional tfvars file for external inputs (repeatable)")
	flags.StringArrayVar(&f.vars, "var", nil, "external input value as name=value (repeatable)")
	flags.BoolVar(&f.createTfvars, "create-tfvars", false, "materialize generated.auto.tfvars in each consumer root — the standalone wiring for detached use (default threads values in memory only)")
	flags.StringArrayVar(&f.backendConfig, "backend-config", nil, "out-of-band backend config value for init, as key=value (repeatable)")
	flags.StringVar(&f.output, "output", "text", "report format: text or json")
	flags.BoolVarP(&f.interactive, "interactive", "i", false, "guided walkthrough of the whole migration")
	flags.BoolVarP(&f.yes, "yes", "y", false, "approve the migration automatically instead of pausing for confirmation after prove")

	cmd.AddCommand(migratePlanCmd(), migrateProveCmd(), migrateRunCmd(), migrateVerifyCmd())
	return cmd
}

func migratePlanCmd() *cobra.Command {
	var f migrateFlags
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Materialize the migration: pull read-only, back up, carve local state copies, write a receipt",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigratePlan(cmd.Context(), f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.engine, "engine", "", "state engine: terraform or tofu (required)")
	flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	flags.StringVar(&f.stateFile, "state-file", "", "carve this state snapshot instead of pulling from the configured backend")
	flags.StringVar(&f.output, "output", "text", "report format: text or json")
	flags.BoolVarP(&f.interactive, "interactive", "i", false, "guided walkthrough: prompt for root/engine/state source, preview the moves, confirm")
	return cmd
}

// loadRunManifest loads the canonical manifest and requires it to be executed
// (refactor run) and unchanged since (staleness checksum).
func loadRunManifest(rootDir string) (*manifest.Manifest, error) {
	path := manifest.Path(rootDir)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no %s manifest found in %s; run `demonolith refactor` first", manifest.FileName, rootDir)
	}
	m, err := manifest.Load(path)
	if err != nil {
		return nil, err
	}
	if !m.IsRun() {
		return nil, fmt.Errorf("manifest %s is planned but not run; run `demonolith refactor run` first", manifest.FileName)
	}
	sum, err := manifest.Checksum(m.ChecksumDirs(rootDir))
	if err != nil {
		return nil, verdictf("manifest %s: emitted roots unreadable: %v", manifest.FileName, err)
	}
	if sum != m.EmitChecksum {
		return nil, verdictf("manifest %s is stale: the emitted roots changed after it was written; re-run `demonolith refactor`", manifest.FileName)
	}
	return m, nil
}

// analyzeMatching re-analyzes the source and requires it to still match the
// manifest — the boundary the proof threads over must describe the same plan.
func analyzeMatching(rootDir string, m *manifest.Manifest) (*pipeline.Analysis, error) {
	a, err := pipeline.Analyze(rootDir, pipeline.Options{Remainder: m.Source.RemainderModule})
	if err != nil {
		return nil, err
	}
	fresh, err := freshSemantic(a, rootDir, m)
	if err != nil {
		return nil, err
	}
	if !manifest.SemanticEqual(fresh, m) {
		return nil, verdictf("manifest %s does not match the source analysis; re-run `demonolith refactor`", manifest.FileName)
	}
	return a, nil
}

// migratePlanReport records the plan (carve) result.
type migratePlanReport struct {
	Manifest     string            `json:"manifest"`
	Skipped      bool              `json:"skipped"`
	SkipReason   string            `json:"skip_reason,omitempty"`
	Moves        []moveReport      `json:"moves,omitempty"`
	ModuleStates map[string]string `json:"module_states,omitempty"`
	BackupPath   string            `json:"backup_path,omitempty"`
	ReceiptPath  string            `json:"receipt_path,omitempty"`
}

type moveReport struct {
	Address string `json:"address"`
	Module  string `json:"module"`
	Outcome string `json:"outcome"` // moved | skipped
}

func runMigratePlan(ctx context.Context, f migrateFlags) error {
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
		outln("Interactive migrate plan — Enter keeps the value in brackets.")
		rootIn, err := promptString("Monolith root", f.rootDir)
		if err != nil {
			return err
		}
		f.rootDir = rootIn
	}
	rootDir := resolveRoot(f.rootDir)
	m, err := loadRunManifest(rootDir)
	if err != nil {
		return err
	}

	if f.interactive {
		if f.engine == "" && f.execPath == "" {
			engine, err := promptEngine()
			if err != nil {
				return err
			}
			f.engine = engine
		}
		if f.stateFile == "" {
			sf, err := promptLine("State snapshot file (Enter = pull from the configured backend): ")
			if err != nil {
				return err
			}
			f.stateFile = sf
		}
		ok, err := confirmMigratePlan(rootDir, m, f)
		if err != nil {
			return err
		}
		if !ok {
			outln("Aborted; nothing executed.")
			return nil
		}
	}
	execPath, err := engineExecPath(f.engine, f.execPath)
	if err != nil {
		return err
	}

	rep, err := migrateCarve(ctx, rootDir, m, execPath, f)
	var verdict error
	if err != nil {
		if ExitCode(err) != ExitVerdict {
			return err
		}
		verdict = err
		rep = &migratePlanReport{Manifest: manifest.FileName, Skipped: true, SkipReason: err.Error()}
	}

	if mode == outputJSON {
		if err := printJSON(rep); err != nil {
			return err
		}
		return verdict
	}
	printMigratePlanReport(rootDir, *rep)
	return verdict
}

// migrateCarve performs the local carve and writes the plan receipt.
func migrateCarve(ctx context.Context, rootDir string, m *manifest.Manifest, execPath string, f migrateFlags) (*migratePlanReport, error) {
	rep := &migratePlanReport{Manifest: manifest.FileName}

	// Idempotency: a generation whose plan receipt records a complete carve is
	// skipped, so re-running resumes rather than erroring.
	receipt, err := manifest.LatestReceiptFor(rootDir, m.EmitChecksum, manifest.ActionPlan)
	if err != nil {
		return nil, err
	}
	if receipt != nil && receipt.Complete {
		rep.Skipped = true
		rep.SkipReason = "already carved (complete plan receipt found)"
		return rep, nil
	}

	plan := m.Plan()
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
	filtered, outcomes, err := filterApplied(prep, workDir, plan, manifest.FileName)
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
		Version:          manifest.SchemaVersion,
		Created:          now.UTC().Format(time.RFC3339),
		Tool:             toolString(),
		Manifest:         manifest.FileName,
		ManifestChecksum: m.EmitChecksum,
		Action:           manifest.ActionPlan,
		Engine:           f.engine,
		Complete:         true,
		ModuleStates:     map[string]string{},
		BackupPath:       relForReceipt(rootDir, res.BackupPath),
	}
	for mod, st := range res.ModuleStates {
		r.ModuleStates[mod] = relForReceipt(rootDir, st)
	}
	for _, mv := range rep.Moves {
		r.Moves = append(r.Moves, manifest.MoveOutcome(mv))
	}
	receiptPath, err := manifest.WriteReceipt(r, rootDir)
	if err != nil {
		return nil, err
	}
	rep.ReceiptPath = receiptPath
	return rep, nil
}

// planReceiptStates loads the current generation's complete plan receipt and
// resolves its carved state paths, requiring every file (and the backup) to be
// intact.
func planReceiptStates(rootDir string, m *manifest.Manifest) (*manifest.Receipt, map[string]string, string, error) {
	receipt, err := manifest.LatestReceiptFor(rootDir, m.EmitChecksum, manifest.ActionPlan)
	if err != nil {
		return nil, nil, "", err
	}
	if receipt == nil || !receipt.Complete {
		return nil, nil, "", fmt.Errorf("no complete migrate plan for this manifest generation; run `demonolith migrate plan` first")
	}
	states := map[string]string{}
	for mod, p := range receipt.ModuleStates {
		rp := manifest.Resolve(rootDir, p)
		if _, err := os.Stat(rp); err != nil {
			return nil, nil, "", fmt.Errorf("carved state for module %q missing (%s); re-run `demonolith migrate plan`", mod, rp)
		}
		states[mod] = rp
	}
	backup := manifest.Resolve(rootDir, receipt.BackupPath)
	if _, err := os.Stat(backup); err != nil {
		return nil, nil, "", fmt.Errorf("state backup missing (%s); re-run `demonolith migrate plan`", backup)
	}
	return receipt, states, backup, nil
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

func printMigratePlanReport(rootDir string, rep migratePlanReport) {
	if rep.Skipped {
		outf("%s: skipped — %s\n", rep.Manifest, rep.SkipReason)
		return
	}
	outf("%s: carved\n", rep.Manifest)
	for _, mv := range rep.Moves {
		outf("  %-8s %-40s -> %s\n", mv.Outcome, mv.Address, mv.Module)
	}
	outln("\nCarved state (local copies, nothing pushed yet):")
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

// runMigratePipeline is the bare `demonolith migrate`: plan → prove → run →
// verify, run's prove-verdict guard satisfied by the prove step.
func runMigratePipeline(ctx context.Context, f migrateFlags) error {
	steps := []struct {
		name    string
		fn      func() error
		confirm bool
	}{
		{"migrate plan", func() error { return runMigratePlan(ctx, f) }, false},
		{"migrate prove", func() error { return runMigrateProve(ctx, f) }, false},
		{"migrate run", func() error { return runMigrateRun(ctx, f) }, true},
		{"migrate verify", func() error { return runMigrateVerify(ctx, f) }, false},
	}
	for _, s := range steps {
		if s.confirm && !f.yes {
			if !stdinIsTTY() {
				return fmt.Errorf("the pipeline pauses for approval after prove; pass -y to approve automatically, or run the subcommands individually")
			}
			ok, err := promptYesNo("\nProceed with the migration (seed the state destinations)?", false)
			if err != nil {
				return err
			}
			if !ok {
				outln("Stopped before migrate run; the carve and proof are in place.")
				return nil
			}
		}
		outf("── %s ──\n", s.name)
		if err := s.fn(); err != nil {
			return err
		}
		outln("")
	}
	return nil
}
