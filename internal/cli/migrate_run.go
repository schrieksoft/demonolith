package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/spf13/cobra"

	"github.com/schrieksoft/demonolith/internal/manifest"
)

func migrateRunCmd() *cobra.Command {
	var f migrateFlags
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Execute the migration: seed each module's derived backend with its carved state (guarded, never forced)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrateRun(cmd.Context(), f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.engine, "engine", "", "state engine: terraform or tofu (required unless the monolith has no backend)")
	flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	flags.StringArrayVar(&f.backendConfig, "backend-config", nil, "out-of-band backend config value for init, as key=value (repeatable)")
	flags.BoolVar(&f.unproven, "unproven", false, "skip the prove-verdict precondition (explicitly run an unproven migration)")
	flags.StringVar(&f.output, "output", "text", "report format: text or json")
	flags.BoolVarP(&f.interactive, "interactive", "i", false, "confirm the per-module destinations before pushing")
	return cmd
}

// migrateRunReport records where each module's state landed.
type migrateRunReport struct {
	Manifest    string                 `json:"manifest"`
	Pushes      []manifest.PushOutcome `json:"pushes"`
	ReceiptPath string                 `json:"receipt_path"`
}

// runMigrateRun executes the migration: for every module, seed its state
// destination with the carved file from migrate plan. Preconditions: a
// complete plan receipt and a passing prove verdict no older than it (unless
// --unproven). Targets must be empty; nothing is ever forced; the monolith's
// own state is never written.
func runMigrateRun(ctx context.Context, f migrateFlags) error {
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
	m, err := loadRunManifest(rootDir)
	if err != nil {
		return err
	}
	planReceipt, moduleStates, _, err := planReceiptStates(rootDir, m)
	if err != nil {
		return err
	}

	if !f.unproven {
		v, err := manifest.LatestProveVerdict(rootDir, m.EmitChecksum)
		if err != nil {
			return err
		}
		if v == nil {
			return fmt.Errorf("no prove verdict for this manifest generation; run `demonolith migrate prove` first (or pass --unproven)")
		}
		if !v.OK {
			return verdictf("the prove verdict for this generation is negative; fix the plan before running")
		}
		if older(v.Created, planReceipt.Created) {
			return fmt.Errorf("the prove verdict predates the migrate plan; re-run `demonolith migrate prove` (or pass --unproven)")
		}
	}

	modules := make([]string, 0, len(moduleStates))
	for name := range moduleStates {
		modules = append(modules, name)
	}
	sort.Strings(modules)

	if f.interactive {
		outln("State destinations:")
		for _, name := range modules {
			outf("  %-16s %s\n", name, destinationLabel(m, name))
		}
		ok, err := promptYesNo("Seed these destinations from the carved state copies (empty targets only, never forced)?", false)
		if err != nil {
			return err
		}
		if !ok {
			outln("Aborted; nothing pushed.")
			return nil
		}
	}

	rep := migrateRunReport{Manifest: manifest.FileName}
	for _, name := range modules {
		var outcome manifest.PushOutcome
		if m.Backend == nil {
			outcome, err = seedLocal(m, rootDir, name, moduleStates[name])
		} else {
			outcome, err = seedBackend(ctx, m, rootDir, name, moduleStates[name], f)
		}
		if err != nil {
			return fmt.Errorf("module %s: %w", name, err)
		}
		rep.Pushes = append(rep.Pushes, outcome)
	}

	now := time.Now()
	r := &manifest.Receipt{
		Version:          manifest.SchemaVersion,
		Created:          now.UTC().Format(time.RFC3339),
		Tool:             toolString(),
		Manifest:         manifest.FileName,
		ManifestChecksum: m.EmitChecksum,
		Action:           manifest.ActionRun,
		Engine:           f.engine,
		Complete:         true,
		ModuleStates:     planReceipt.ModuleStates,
		Pushes:           rep.Pushes,
	}
	receiptPath, err := manifest.WriteReceipt(r, rootDir)
	if err != nil {
		return err
	}
	rep.ReceiptPath = receiptPath

	if mode == outputJSON {
		return printJSON(rep)
	}
	outln("Migration executed:")
	for _, p := range rep.Pushes {
		outf("  %-16s %-8s %s\n", p.Module, p.Outcome, p.Location)
	}
	outf("  receipt: %s\n", displayPath(rootDir, rep.ReceiptPath))
	outln("\nThe monolith's own state was not touched; retiring it is the cutover step.")
	return nil
}

// destinationLabel renders where a module's state will land.
func destinationLabel(m *manifest.Manifest, module string) string {
	if m.Backend == nil {
		return filepath.Join(m.Modules[module].Dir, "terraform.tfstate") + " (local)"
	}
	return fmt.Sprintf("%s (%s backend)", m.Backend.Modules[module], m.Backend.Type)
}

// seedLocal places the carved state as the root's local state file. An
// existing identical-lineage state is an idempotent skip; anything else is a
// refusal.
func seedLocal(m *manifest.Manifest, rootDir, module, carved string) (manifest.PushOutcome, error) {
	dest := filepath.Join(m.ModuleDirs(rootDir)[module], "terraform.tfstate")
	out := manifest.PushOutcome{Module: module, Location: relForReceipt(rootDir, dest)}
	if _, err := os.Stat(dest); err == nil {
		same, err := sameLineage(dest, carved)
		if err != nil {
			return out, err
		}
		if !same {
			return out, fmt.Errorf("destination %s already holds unrelated state; refusing to overwrite", dest)
		}
		out.Outcome = "skipped"
		return out, nil
	}
	b, err := os.ReadFile(carved)
	if err != nil {
		return out, err
	}
	if err := os.WriteFile(dest, b, 0o600); err != nil {
		return out, err
	}
	out.Outcome = "pushed"
	return out, nil
}

// seedBackend inits the module's derived backend and pushes the carved state
// into it. The target must be empty (or already hold this exact lineage, an
// idempotent skip); push is never forced.
func seedBackend(ctx context.Context, m *manifest.Manifest, rootDir, module, carved string, f migrateFlags) (manifest.PushOutcome, error) {
	out := manifest.PushOutcome{Module: module, Location: m.Backend.Modules[module]}
	execPath, err := engineExecPath(f.engine, f.execPath)
	if err != nil {
		return out, err
	}
	dir := m.ModuleDirs(rootDir)[module]
	tf, err := tfexec.NewTerraform(dir, execPath)
	if err != nil {
		return out, err
	}
	initOpts := []tfexec.InitOption{tfexec.Backend(true)}
	for _, bc := range f.backendConfig {
		initOpts = append(initOpts, tfexec.BackendConfig(bc))
	}
	if err := tf.Init(ctx, initOpts...); err != nil {
		return out, fmt.Errorf("init: %w", err)
	}
	current, err := tf.StatePull(ctx)
	if err != nil {
		return out, fmt.Errorf("state pull: %w", err)
	}
	if hasResources([]byte(current)) {
		same, err := sameLineageRaw([]byte(current), carved)
		if err != nil {
			return out, err
		}
		if !same {
			return out, fmt.Errorf("target %s already holds unrelated state; refusing to push (never forced)", out.Location)
		}
		out.Outcome = "skipped"
		return out, nil
	}
	if err := tf.StatePush(ctx, carved); err != nil {
		return out, fmt.Errorf("state push: %w", err)
	}
	out.Outcome = "pushed"
	return out, nil
}

// older reports whether RFC3339 timestamp a is strictly before b; unparseable
// timestamps count as older (fail safe).
func older(a, b string) bool {
	ta, errA := time.Parse(time.RFC3339, a)
	tb, errB := time.Parse(time.RFC3339, b)
	if errA != nil || errB != nil {
		return true
	}
	return ta.Before(tb)
}

// stateMeta is the subset of a state file the guards read.
type stateMeta struct {
	Lineage   string `json:"lineage"`
	Resources []any  `json:"resources"`
}

func readStateMeta(b []byte) (*stateMeta, error) {
	var s stateMeta
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	return &s, nil
}

func hasResources(b []byte) bool {
	s, err := readStateMeta(b)
	return err == nil && len(s.Resources) > 0
}

func sameLineage(pathA, pathB string) (bool, error) {
	a, err := os.ReadFile(pathA)
	if err != nil {
		return false, err
	}
	return sameLineageRaw(a, pathB)
}

func sameLineageRaw(a []byte, pathB string) (bool, error) {
	b, err := os.ReadFile(pathB)
	if err != nil {
		return false, err
	}
	ma, err := readStateMeta(a)
	if err != nil {
		return false, err
	}
	mb, err := readStateMeta(b)
	if err != nil {
		return false, err
	}
	return ma.Lineage != "" && ma.Lineage == mb.Lineage, nil
}
