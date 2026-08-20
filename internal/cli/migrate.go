package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/schrieksoft/demonolith/internal/boundary"
	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/manifest"
	"github.com/schrieksoft/demonolith/internal/pipeline"
	"github.com/schrieksoft/demonolith/internal/statemove"
	"github.com/schrieksoft/demonolith/internal/statevars"
)

// migrateFlags is the union of the migrate subcommands' flags; the bare parent
// pipeline takes them all.
type migrateFlags struct {
	rootDir       string
	engine        string
	execPath      string
	stateFile     string
	interactive   bool
	refresh       bool
	noTfvars      bool
	varFiles      []string
	vars          []string
	backendConfig []string
	unproven      bool
	overwrite     bool
	yes           bool
}

func migrateCmd() *cobra.Command {
	var f migrateFlags
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate the state: map → prove → run → verify",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigratePipeline(cmd.Context(), f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.engine, "engine", "", "state engine: terraform or tofu (required)")
	flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	flags.StringVar(&f.stateFile, "state-file", "", "split this local state file instead of pulling from the configured backend")
	flags.StringArrayVar(&f.varFiles, "var-file", nil, "additional tfvars file for external inputs (repeatable)")
	flags.StringArrayVar(&f.vars, "var", nil, "external input value as name=value (repeatable)")
	flags.BoolVar(&f.noTfvars, "no-tfvars", false, "do not write demono.root.tfvars/demono.graph.tfvars; pass all values in memory only (for tests)")
	flags.StringArrayVar(&f.backendConfig, "backend-config", nil, "extra backend config passed to init, as key=value (repeatable; for settings that live outside the backend block)")
	flags.BoolVar(&f.overwrite, "overwrite", false, "replace a destination whose existing state does not match this migration (state push -force); the existing state is lost — default refuses")
	flags.BoolVarP(&f.interactive, "interactive", "i", false, "guided walkthrough: engine, state source, variable values and their sources, backend config, ambient credentials — then the pipeline")
	flags.BoolVarP(&f.yes, "yes", "y", false, "approve the migration automatically instead of pausing for confirmation after prove")

	cmd.AddCommand(migrateMapCmd(), migrateProveCmd(), migrateRunCmd(), migrateVerifyCmd())
	return cmd
}

func migrateMapCmd() *cobra.Command {
	var f migrateFlags
	cmd := &cobra.Command{
		Use:   "map",
		Short: "Prepare the migration: pull the state read-only, back it up, split it into per-module local copies, write the map receipt",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrateMap(cmd.Context(), f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.engine, "engine", "", "state engine: terraform or tofu (required)")
	flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	flags.StringVar(&f.stateFile, "state-file", "", "split this local state file instead of pulling from the configured backend")
	flags.BoolVarP(&f.interactive, "interactive", "i", false, "guided walkthrough: prompt for root/engine/state source, preview the moves, confirm")
	return cmd
}

// loadRunManifest loads the canonical manifest and requires it to be executed
// (refactor run) and unchanged since (staleness checksum).
func loadRunManifest(rootDir string) (*manifest.Manifest, error) {
	path := manifest.Path(rootDir)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no %s found in %s; run `demonolith refactor` first", manifest.FileName, rootDir)
	}
	m, err := manifest.Load(path)
	if err != nil {
		return nil, err
	}
	if !m.IsRun() {
		return nil, fmt.Errorf("map %s has not been run yet; run `demonolith refactor run` first", manifest.FileName)
	}
	sum, err := manifest.Checksum(m.ChecksumDirs(rootDir))
	if err != nil {
		return nil, verdictf("map %s: module directories unreadable: %v", manifest.FileName, err)
	}
	if sum != m.EmitChecksum {
		return nil, verdictf("map %s is stale: the module directories changed after it was written; re-run `demonolith refactor`", manifest.FileName)
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
		return nil, verdictf("map %s does not match the source analysis; re-run `demonolith refactor`", manifest.FileName)
	}
	return a, nil
}

// migrateMapReport records the plan (carve) result.
type migrateMapReport struct {
	Manifest     string
	Skipped      bool
	SkipReason   string
	Moves        []moveReport
	ModuleStates map[string]string
	BackupPath   string
	ReceiptPath  string
}

type moveReport struct {
	Address string
	Module  string
	Outcome string // moved | skipped
}

func runMigrateMap(ctx context.Context, f migrateFlags) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if f.interactive {
		if !stdinIsTTY() {
			return fmt.Errorf("--interactive requires a terminal")
		}
		outln("Interactive migrate map — Enter keeps the value in brackets.")
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
			sf, err := promptLine("Local .tfstate copy to split, if you have one (Enter = pull the monolith's state from its backend): ")
			if err != nil {
				return err
			}
			f.stateFile = sf
		}
		ok, err := confirmMigrateMap(rootDir, m, f)
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
		rep = &migrateMapReport{Manifest: manifest.FileName, Skipped: true, SkipReason: err.Error()}
	}

	printMigratePlanReport(rootDir, *rep)
	return verdict
}

// migrateCarve performs the local carve and writes the map receipt.
func migrateCarve(ctx context.Context, rootDir string, m *manifest.Manifest, execPath string, f migrateFlags) (*migrateMapReport, error) {
	rep := &migrateMapReport{Manifest: manifest.FileName}

	// Idempotency: a generation whose map receipt records a complete carve is
	// skipped, so re-running resumes rather than erroring — but only while the
	// recorded carve artifacts still exist. A lost workdir means re-carve, or
	// the receipt points every later step at files that are gone.
	receipt, err := manifest.LatestReceiptFor(rootDir, m.EmitChecksum, manifest.ActionMap)
	if err != nil {
		return nil, err
	}
	if receipt != nil && receipt.Complete && carveArtifactsExist(rootDir, receipt) {
		rep.Skipped = true
		rep.SkipReason = "already split (complete map receipt found)"
		return rep, nil
	}

	plan := m.Plan()
	workDir := filepath.Join(m.OutDir(rootDir), ".demono")
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
		return nil, fmt.Errorf("state split: %w", err)
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
		Action:           manifest.ActionMap,
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

// carveArtifactsExist reports whether every carved state file a map receipt
// records is still on disk (paths are stored root-relative when possible).
func carveArtifactsExist(rootDir string, r *manifest.Receipt) bool {
	if len(r.ModuleStates) == 0 {
		return false
	}
	for _, p := range r.ModuleStates {
		if !filepath.IsAbs(p) {
			p = filepath.Join(rootDir, p)
		}
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}

// mapReceiptStates loads the current generation's complete map receipt and
// resolves its carved state paths, requiring every file (and the backup) to be
// intact.
func mapReceiptStates(rootDir string, m *manifest.Manifest) (*manifest.Receipt, map[string]string, string, error) {
	receipt, err := manifest.LatestReceiptFor(rootDir, m.EmitChecksum, manifest.ActionMap)
	if err != nil {
		return nil, nil, "", err
	}
	if receipt == nil || !receipt.Complete {
		return nil, nil, "", fmt.Errorf("no completed migrate map for this map; run `demonolith migrate map` first")
	}
	states := map[string]string{}
	for mod, p := range receipt.ModuleStates {
		rp := manifest.Resolve(rootDir, p)
		if _, err := os.Stat(rp); err != nil {
			return nil, nil, "", fmt.Errorf("state copy for module %q missing (%s); re-run `demonolith migrate map`", mod, rp)
		}
		states[mod] = rp
	}
	backup := manifest.Resolve(rootDir, receipt.BackupPath)
	if _, err := os.Stat(backup); err != nil {
		return nil, nil, "", fmt.Errorf("state backup missing (%s); re-run `demonolith migrate map`", backup)
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
				return nil, nil, verdictf("map %s does not match the state: %s is in neither the monolith state nor module %s's state", name, mv.SourceAddr, mod)
			}
		}
	}
	return filtered, outcomes, nil
}

// materializeBackendEnv writes each module's gitignored .env from the
// monolith root's init-time resolved backend config — credentials as the
// engines' official environment variables, sourced by run/verify around each
// module's init. A migration-time concern: the credentials exist because the
// root was init'd, so the migrate family owns them, not refactor.
func materializeBackendEnv(rootDir string, m *manifest.Manifest) error {
	if m.Backend == nil {
		return nil
	}
	block, err := emit.ParseBackend(rootDir)
	if err != nil || block == nil {
		return err
	}
	wrote := false
	for _, dir := range m.ModuleDirs(rootDir) {
		w, err := emit.WriteEnvFile(dir, block.CredentialEnv())
		if err != nil {
			return err
		}
		wrote = wrote || w
	}
	if wrote {
		outln("Backend credentials written to per-module demono.env files")
	}
	return nil
}

// materializeRootTfvars writes each module's demono.root.tfvars: the root
// variable values the module declares, resolved in the engine's own
// precedence (TF_VAR_* environment, root tfvars files, --var-file, --var;
// declared defaults travel in the carved code and need no entry). --no-tfvars
// skips the file and returns the values for in-memory threading only.
func materializeRootTfvars(rootDir string, m *manifest.Manifest, bound *boundary.Result, f migrateFlags) (*statevars.Result, error) {
	varVals, err := collectVarValues(rootDir, f.varFiles, f.vars, nil)
	if err != nil {
		return nil, err
	}
	moduleDirs := m.ModuleDirs(rootDir)

	rootVals := map[string]map[string]string{}
	for name, dir := range moduleDirs {
		declared, err := moduleVarNames(dir)
		if err != nil {
			return nil, err
		}
		cross := map[string]bool{}
		if b := bound.Boundaries[name]; b != nil {
			for inName, in := range b.Inputs {
				if !in.External {
					cross[inName] = true
				}
			}
		}
		for v := range declared {
			if cross[v] {
				continue
			}
			if val, ok := varVals[v]; ok {
				if rootVals[name] == nil {
					rootVals[name] = map[string]string{}
				}
				rootVals[name][v] = val
			}
		}
	}
	if f.noTfvars {
		return statevars.Collect(rootVals, nil), nil
	}
	return statevars.WriteRoot(moduleDirs, rootVals)
}

// materializeGraphTfvars writes each module's demono.graph.tfvars: its
// cross-module input values resolved from the applied monolith state, with
// inputs state cannot resolve (child-module outputs) filled from the values
// the proof threaded out of producer plans. What remains unresolved (an
// --unproven run with no proof values) is listed in the result rather than
// failing. --no-tfvars skips the file. Returns how many values came from the
// proof.
func materializeGraphTfvars(rootDir string, m *manifest.Manifest, bound *boundary.Result, f migrateFlags) (*statevars.Result, int, error) {
	_, _, backup, err := mapReceiptStates(rootDir, m)
	if err != nil {
		return nil, 0, err
	}
	st, err := statevars.LoadState(backup)
	if err != nil {
		return nil, 0, fmt.Errorf("tfvars: %w", err)
	}
	crossVals, unresolved := statevars.ResolveCross(st, bound, statevars.Options{SourceDir: rootDir})
	filled := 0
	if len(unresolved) > 0 {
		if th := loadProofThreaded(rootDir, m); th != nil {
			remaining := unresolved[:0]
			for _, u := range unresolved {
				if v, ok := th[u.Consumer][u.Input]; ok {
					if crossVals[u.Consumer] == nil {
						crossVals[u.Consumer] = map[string]string{}
					}
					crossVals[u.Consumer][u.Input] = v
					filled++
					continue
				}
				remaining = append(remaining, u)
			}
			unresolved = remaining
		}
	}
	sv := statevars.Collect(nil, crossVals)
	if !f.noTfvars {
		sv, err = statevars.WriteGraph(m.ModuleDirs(rootDir), crossVals)
		if err != nil {
			return nil, 0, err
		}
	}
	for _, u := range unresolved {
		sv.Unresolved = append(sv.Unresolved, u.String())
	}
	return sv, filled, nil
}

// proofThreadedFile is the gitignored workdir sidecar where prove records the
// cross-module values it threaded from producer plans, for migrate run to
// fill into graph-tfvars entries that state cannot resolve.
const proofThreadedFile = "proof-threaded.json"

type proofThreaded struct {
	ManifestChecksum string                       `json:"manifest_checksum"`
	Threaded         map[string]map[string]string `json:"threaded"`
}

func proofThreadedPath(rootDir string, m *manifest.Manifest) string {
	return filepath.Join(m.OutDir(rootDir), ".demono", proofThreadedFile)
}

// writeProofThreaded persists the proof's threaded values for this manifest
// generation. Best effort by design: the values improve run's graph tfvars
// but are never required.
func writeProofThreaded(rootDir string, m *manifest.Manifest, threaded map[string]map[string]string) error {
	if len(threaded) == 0 {
		return nil
	}
	p := proofThreadedPath(rootDir, m)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(proofThreaded{ManifestChecksum: m.EmitChecksum, Threaded: threaded}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// loadProofThreaded reads the sidecar when it exists and matches the current
// manifest generation; nil otherwise.
func loadProofThreaded(rootDir string, m *manifest.Manifest) map[string]map[string]string {
	b, err := os.ReadFile(proofThreadedPath(rootDir, m))
	if err != nil {
		return nil
	}
	var pt proofThreaded
	if err := json.Unmarshal(b, &pt); err != nil || pt.ManifestChecksum != m.EmitChecksum {
		return nil
	}
	return pt.Threaded
}

// relForReceipt stores receipt paths relative to rootDir when possible.
func relForReceipt(rootDir, p string) string {
	rel, err := filepath.Rel(rootDir, p)
	if err != nil || len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
		return p
	}
	return rel
}

func printMigratePlanReport(rootDir string, rep migrateMapReport) {
	if rep.Skipped {
		outf("%s %s\n", heading("Splitting the state:"), warn("skipped — "+rep.SkipReason))
		return
	}
	outln(heading("Splitting the state") + " (moves from " + rep.Manifest + "):")
	for _, mv := range rep.Moves {
		oc := fmt.Sprintf("%-8s", mv.Outcome)
		if mv.Outcome == "moved" {
			oc = success(oc)
		} else {
			oc = warn(oc)
		}
		outf("  %s %-40s -> %s\n", oc, mv.Address, mv.Module)
	}
	outln("\n" + heading("Per-module state files written") + " (local copies, nothing pushed yet):")
	mods := make([]string, 0, len(rep.ModuleStates))
	for m := range rep.ModuleStates {
		mods = append(mods, m)
	}
	sort.Strings(mods)
	for _, m := range mods {
		outf("  %-16s %s\n", m, displayPath(rootDir, rep.ModuleStates[m]))
	}
	outf("  backup:  %s\n", displayPath(rootDir, rep.BackupPath))
	outln("\n" + heading("Receipt:"))
	outf("  %s\n", displayPath(rootDir, rep.ReceiptPath))
}

// runMigratePipeline is the bare `demonolith migrate`: plan → prove → run →
// verify, run's prove-verdict guard satisfied by the prove step.
func runMigratePipeline(ctx context.Context, f migrateFlags) error {
	if f.interactive {
		if !stdinIsTTY() {
			return fmt.Errorf("--interactive requires a terminal")
		}
		rootDir, m, err := migrateInputsWizard(&f)
		if err != nil {
			return err
		}
		ok, err := confirmMigrateMap(rootDir, m, f)
		if err != nil {
			return err
		}
		if !ok {
			outln("Aborted; nothing executed.")
			return nil
		}
		// The wizard resolved every choice into flags; the steps themselves
		// run plain. The pause before migrate run still applies.
		f.interactive = false
	}
	steps := []struct {
		name    string
		fn      func() error
		confirm bool
	}{
		{"migrate map", func() error { return runMigrateMap(ctx, f) }, false},
		{"migrate prove", func() error { return runMigrateProve(ctx, f) }, false},
		{"migrate run", func() error { return runMigrateRun(ctx, f) }, true},
		{"migrate verify", func() error { return runMigrateVerify(ctx, f) }, false},
	}
	for _, s := range steps {
		if s.confirm && !f.yes {
			if !stdinIsTTY() {
				return fmt.Errorf("the pipeline pauses for approval after prove; pass -y to approve automatically, or run the subcommands individually")
			}
			ok, err := promptYesNo("\nProceed with the migration (push each module's state to its new location)?", true)
			if err != nil {
				return err
			}
			if !ok {
				outln("Stopped before migrate run; the state copies and proof are in place.")
				return nil
			}
		}
		outf("\n%s\n\n", banner("── "+s.name+" ──"))
		if err := s.fn(); err != nil {
			return err
		}
	}
	return nil
}
