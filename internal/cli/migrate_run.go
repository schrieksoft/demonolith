package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/spf13/cobra"

	"github.com/schrieksoft/demonolith/internal/dotenv"
	"github.com/schrieksoft/demonolith/internal/emit"
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
	flags.StringArrayVar(&f.varFiles, "var-file", nil, "additional tfvars file for external inputs (repeatable)")
	flags.StringArrayVar(&f.vars, "var", nil, "external input value as name=value (repeatable)")
	flags.BoolVar(&f.noTfvars, "no-tfvars", false, "do not materialize demono.root.tfvars/demono.graph.tfvars; thread all values in memory only (for tests)")
	flags.BoolVar(&f.unproven, "unproven", false, "skip the prove-receipt precondition (explicitly run an unproven migration)")
	flags.BoolVar(&f.overwrite, "overwrite", false, "replace a target whose state does not match the carve (state push -force); the occupant is lost — default refuses")
	flags.StringVar(&f.output, "output", "text", "report format: text or json")
	flags.BoolVarP(&f.interactive, "interactive", "i", false, "confirm the per-module destinations before pushing")
	return cmd
}

// migrateRunReport records where each module's state landed.
type migrateRunReport struct {
	Manifest        string                 `json:"map"`
	Pushes          []manifest.PushOutcome `json:"pushes"`
	ReceiptPath     string                 `json:"receipt_path"`
	TfvarsFiles     map[string]string      `json:"tfvars_files,omitempty"`
	UnresolvedGraph []string               `json:"unresolved_graph,omitempty"`
	FilledFromProof int                    `json:"filled_from_proof,omitempty"`
}

// runMigrateRun executes the migration: for every module, seed its state
// destination with the carved file from migrate map. Preconditions: a
// complete map receipt and a passing prove verdict no older than it (unless
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
	mapReceipt, moduleStates, _, err := mapReceiptStates(rootDir, m)
	if err != nil {
		return err
	}

	if !f.unproven {
		v, err := manifest.LatestProveVerdict(rootDir, m.EmitChecksum)
		if err != nil {
			return err
		}
		if v == nil {
			return fmt.Errorf("no prove receipt for this map generation; run `demonolith migrate prove` first (or pass --unproven)")
		}
		if !v.OK {
			return verdictf("the prove receipt for this generation is negative; fix the map before running")
		}
		if older(v.Created, mapReceipt.Created) {
			return fmt.Errorf("the prove receipt predates the migrate map; re-run `demonolith migrate prove` (or pass --unproven)")
		}
	}

	a, err := analyzeMatching(rootDir, m)
	if err != nil {
		return err
	}
	if err := materializeBackendEnv(rootDir, m); err != nil {
		return err
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
		prompt := "Seed these destinations from the carved state copies (empty targets only, never forced)?"
		if f.overwrite {
			prompt = "Seed these destinations from the carved state copies (--overwrite: non-matching occupants will be REPLACED)?"
		}
		ok, err := promptYesNo(prompt, false)
		if err != nil {
			return err
		}
		if !ok {
			outln("Aborted; nothing pushed.")
			return nil
		}
	}

	writeRunReceipt := func(complete bool, pushes []manifest.PushOutcome) (string, error) {
		r := &manifest.Receipt{
			Version:          manifest.SchemaVersion,
			Created:          time.Now().UTC().Format(time.RFC3339),
			Tool:             toolString(),
			Manifest:         manifest.FileName,
			ManifestChecksum: m.EmitChecksum,
			Action:           manifest.ActionRun,
			Engine:           f.engine,
			Complete:         complete,
			ModuleStates:     mapReceipt.ModuleStates,
			Pushes:           pushes,
		}
		return manifest.WriteReceipt(r, rootDir)
	}

	rep := migrateRunReport{Manifest: manifest.FileName}
	if mode == outputText {
		if f.overwrite {
			outln("\n" + heading("Seeding state destinations") + " (--overwrite: non-matching occupants will be replaced):")
		} else {
			outln("\n" + heading("Seeding state destinations") + " (empty targets only, never forced):")
		}
	}
	for _, name := range modules {
		if mode == outputText {
			outf("  %s: seeding %s ... ", name, destinationLabel(m, name))
		}
		var outcome manifest.PushOutcome
		if m.Backend == nil {
			outcome, err = seedLocal(m, rootDir, name, moduleStates[name], f)
		} else {
			outcome, err = seedBackend(ctx, m, rootDir, name, moduleStates[name], f)
		}
		if err != nil {
			if mode == outputText {
				outln(fail("FAILED"))
			}
			// Record how far the run got — but never demote a complete receipt
			// of this generation to a partial one on a failed retry.
			prev, perr := manifest.LatestReceiptFor(rootDir, m.EmitChecksum, manifest.ActionRun)
			if perr == nil && (prev == nil || !prev.Complete) {
				if rp, werr := writeRunReceipt(false, rep.Pushes); werr == nil && mode == outputText {
					outf("  %d of %d modules done; partial run receipt: %s\n", len(rep.Pushes), len(modules), displayPath(rootDir, rp))
				}
			}
			return fmt.Errorf("module %s: %w", name, err)
		}
		if mode == outputText {
			label := outcome.Outcome
			switch label {
			case "pushed":
				label = success("pushed")
			case "skipped":
				label = warn("skipped (target already holds this module's state)")
			case "overwritten":
				label = fail("OVERWRITTEN — replaced a non-matching occupant")
			}
			outln(label)
		}
		rep.Pushes = append(rep.Pushes, outcome)
	}

	receiptPath, err := writeRunReceipt(true, rep.Pushes)
	if err != nil {
		return err
	}
	rep.ReceiptPath = receiptPath

	// An overwrite destroyed someone's state; warn unconditionally, on stderr
	// so JSON consumers see it too without it polluting the report.
	var overwrote []string
	for _, p := range rep.Pushes {
		if p.Outcome == "overwritten" {
			overwrote = append(overwrote, p.Module)
		}
	}
	if len(overwrote) > 0 {
		fmt.Fprintf(os.Stderr, "WARNING: --overwrite replaced non-matching state at %d target(s): %s. The previous occupants are gone.\n", len(overwrote), strings.Join(overwrote, ", "))
	}

	// The graph tfvars are a post-migration artifact for detached use, not an
	// input to the seeding — materialized last, reported right after.
	graph, filledFromProof, err := materializeGraphTfvars(rootDir, m, a.Boundary, f)
	if err != nil {
		return err
	}
	rep.TfvarsFiles = graph.Files
	rep.UnresolvedGraph = graph.Unresolved
	rep.FilledFromProof = filledFromProof

	if mode == outputJSON {
		return printJSON(rep)
	}
	outln("\n" + heading("Migration executed:"))
	for _, p := range rep.Pushes {
		outf("  %-16s %-8s %s\n", p.Module, p.Outcome, p.Location)
	}
	outln("\n" + heading("Receipt:"))
	outf("  %s\n", displayPath(rootDir, rep.ReceiptPath))
	if len(graph.Files) > 0 {
		outln("\n" + heading("Materialized cross-module input values (demono.graph.tfvars):"))
		mods := make([]string, 0, len(graph.Files))
		for name := range graph.Files {
			mods = append(mods, name)
		}
		sort.Strings(mods)
		for _, name := range mods {
			outf("  %-16s %s\n", name, displayPath(rootDir, graph.Files[name]))
		}
	}
	if len(graph.Unresolved) > 0 {
		outln("\nCross-module inputs not in the graph tfvars (child-module outputs are not stored in state); a control plane threads these at runtime — pass as -var when planning a root detached:")
		for _, u := range graph.Unresolved {
			outf("  %s\n", u)
		}
	}
	outln("\nYour original monolith state file remains untouched!")
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
// existing state that matches the carve — same lineage, or identical content
// from a re-carve of the same monolith — is an idempotent skip; anything else
// is a refusal unless --overwrite explicitly sacrifices the occupant.
func seedLocal(m *manifest.Manifest, rootDir, module, carved string, f migrateFlags) (manifest.PushOutcome, error) {
	dest := filepath.Join(m.ModuleDirs(rootDir)[module], "terraform.tfstate")
	out := manifest.PushOutcome{Module: module, Location: relForReceipt(rootDir, dest)}
	overwriting := false
	if _, err := os.Stat(dest); err == nil {
		same, err := sameLineage(dest, carved)
		if err != nil {
			return out, err
		}
		if !same {
			current, rerr := os.ReadFile(dest)
			if rerr != nil {
				return out, rerr
			}
			same, err = sameContent(current, carved)
			if err != nil {
				return out, err
			}
		}
		if same {
			out.Outcome = "skipped"
			return out, nil
		}
		if !f.overwrite {
			return out, fmt.Errorf("destination %s already holds state that does not match this carve; refusing to overwrite. If it is left over from an earlier migration attempt, inspect it and remove it before re-running — or re-run with --overwrite to replace it (the occupant is lost)", dest)
		}
		overwriting = true
	}
	b, err := os.ReadFile(carved)
	if err != nil {
		return out, err
	}
	if err := os.WriteFile(dest, b, 0o600); err != nil {
		return out, err
	}
	out.Outcome = "pushed"
	if overwriting {
		out.Outcome = "overwritten"
	}
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
	env, err := dotenv.Load(filepath.Join(dir, emit.EnvFileName))
	if err != nil {
		return out, err
	}
	restore, err := dotenv.Apply(env)
	if err != nil {
		return out, err
	}
	defer restore()
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
	overwriting := false
	if hasResources([]byte(current)) {
		same, err := sameLineageRaw([]byte(current), carved)
		if err != nil {
			return out, err
		}
		if !same {
			same, err = sameContent([]byte(current), carved)
			if err != nil {
				return out, err
			}
		}
		if same {
			out.Outcome = "skipped"
			return out, nil
		}
		if !f.overwrite {
			return out, fmt.Errorf("target %s already holds state that does not match this carve; refusing to push (not forced by default). If it is left over from an earlier migration attempt, inspect it (`state pull` in %s) and empty that remote state before re-running — or re-run with --overwrite to force-push over it (the occupant is lost)", out.Location, displayPath(rootDir, dir))
		}
		overwriting = true
	}
	var pushOpts []tfexec.StatePushCmdOption
	if overwriting {
		pushOpts = append(pushOpts, tfexec.Force(true))
	}
	if err := tf.StatePush(ctx, carved, pushOpts...); err != nil {
		return out, fmt.Errorf("state push: %w", err)
	}
	out.Outcome = "pushed"
	if overwriting {
		out.Outcome = "overwritten"
	}
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

// sameContent reports whether an occupied target holds the same migration
// payload as the carved file: deep-equal after dropping the identity fields a
// re-carve regenerates (lineage, serial) and the engine version stamp. This
// is what makes a re-run after a re-carve an idempotent skip instead of a
// refusal against your own earlier push.
func sameContent(current []byte, carvedPath string) (bool, error) {
	carved, err := os.ReadFile(carvedPath)
	if err != nil {
		return false, err
	}
	var a, b map[string]any
	if err := json.Unmarshal(current, &a); err != nil {
		return false, fmt.Errorf("parse state: %w", err)
	}
	if err := json.Unmarshal(carved, &b); err != nil {
		return false, fmt.Errorf("parse state %s: %w", carvedPath, err)
	}
	for _, k := range []string{"lineage", "serial", "terraform_version"} {
		delete(a, k)
		delete(b, k)
	}
	return reflect.DeepEqual(a, b), nil
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
