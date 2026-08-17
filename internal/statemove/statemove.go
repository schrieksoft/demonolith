// Package statemove carves a monolith's Terraform state into per-module local
// state files. It operates only on local copies: the monolith state is pulled
// once (or read from a provided local file), split with `state mv
// -state/-state-out`, and the resulting per-module state files are written to
// disk. Nothing is ever pushed to a real backend in v1 — the carved files are
// both the deliverable and the input the proof stage validates against.
//
// Every source state is backed up before mutation, and all moves in a run are
// executed as one batch so a mid-run failure recovers to the pre-run snapshot.
package statemove

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/hashicorp/terraform-exec/tfexec"

	"github.com/schrieksoft/demonolith/internal/hclgraph"
	"github.com/schrieksoft/demonolith/internal/placement"
)

// Engine selects the executable used to drive state operations.
type Engine string

const (
	EngineTerraform Engine = "terraform"
	EngineTofu      Engine = "tofu"
)

// Options configures a carve.
type Options struct {
	// ExecPath is the terraform/tofu binary. If empty, resolved from Engine via
	// PATH.
	ExecPath string
	// Engine picks terraform vs tofu when ExecPath is empty (default terraform).
	Engine Engine
	// SourceStatePath, if set, is a local monolith state file to carve; when
	// empty the monolith state is pulled from its configured backend in SrcDir.
	SourceStatePath string
}

// Plan is the set of state moves derived from placement: for each module, the
// resource addresses (as written) that must be moved into its state, in their
// source and destination address forms. v1 keeps addresses identical across the
// boundary (no de-nesting for flat roots); the fields are distinct so nested
// re-addressing can be added without changing the interface.
type Plan struct {
	// Moves maps a module name to the addresses moved into it.
	Moves map[string][]Move
	// Remainder is the catchall module name; its resources stay in the
	// carved-down monolith state rather than being moved.
	Remainder string
	// AdoptRemainder is true when the remainder holds stateful blocks, so the
	// carved-down monolith state must be adopted as its state file.
	AdoptRemainder bool
}

// Move is one resource relocation.
type Move struct {
	// SourceAddr is the address in the monolith state.
	SourceAddr string
	// DestAddr is the address in the carved module state.
	DestAddr string
}

// Result records what a carve produced.
type Result struct {
	// ModuleStates maps a module name to its carved local state file path.
	ModuleStates map[string]string
	// BackupPath is the pre-run snapshot of the source monolith state.
	BackupPath string
	// Empty lists modules that received no resources (e.g. a data-only module);
	// no state file is written for them.
	Empty []string
}

// BuildPlan derives the state-move plan from placement. Only managed resources
// carry state; data sources are re-read, not moved. A duplicated data source
// therefore contributes no move (its copies are re-read in each module).
func BuildPlan(p *placement.Placement) *Plan {
	plan := &Plan{Moves: map[string][]Move{}, Remainder: p.Remainder}
	for _, module := range p.ModuleNames() {
		for _, addr := range p.Modules[module] {
			// Managed resources and whole module calls carry state; data sources
			// are re-read, not moved. `terraform state mv module.x module.x` moves
			// every resource inside module.x in one operation.
			if addr.Kind != hclgraph.KindResource && addr.Kind != hclgraph.KindModule {
				continue
			}
			a := addr.String()
			plan.Moves[module] = append(plan.Moves[module], Move{SourceAddr: a, DestAddr: a})
			if module == p.Remainder {
				plan.AdoptRemainder = true
			}
		}
	}
	for m := range plan.Moves {
		sort.Slice(plan.Moves[m], func(i, j int) bool {
			return plan.Moves[m][i].SourceAddr < plan.Moves[m][j].SourceAddr
		})
	}
	return plan
}

// Prepared is the local working state Prepare established.
type Prepared struct {
	// MonolithState is the local working copy the moves mutate.
	MonolithState string
	// BackupPath is the pre-carve snapshot of the source state.
	BackupPath string
	// Resumed is true when a prior run's working state was reused.
	Resumed bool
}

// Prepare obtains the monolith state as a local working file in workDir and
// backs it up. If a prior run left a working state in workDir it is reused and
// the existing backup preserved — the working copy is partially carved, so
// re-obtaining the source would make already-executed moves fail or duplicate.
func Prepare(ctx context.Context, srcDir, workDir string, opts Options) (*Prepared, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	monolithState := filepath.Join(workDir, "monolith.tfstate")
	// "demono-backup" (not terraform's default ".backup") so its provenance is
	// unambiguous: this is the deliberate pre-carve snapshot, not one of
	// terraform's automatic per-mutation .tfstate.<ts>.backup files.
	backup := filepath.Join(workDir, "monolith.demono-backup.tfstate")

	if _, err := os.Stat(monolithState); err == nil {
		// Resuming an interrupted carve: the working copy is the truth (a
		// re-pull would resurrect already-moved entries). But it must be the
		// same state we would be carving now — a leftover workDir from an
		// earlier backend or source is a silent wrong-world carve. Verify the
		// lineage against the current source before trusting it.
		if err := verifySameLineage(ctx, srcDir, monolithState, backup, opts); err != nil {
			return nil, err
		}
		return &Prepared{MonolithState: monolithState, BackupPath: backup, Resumed: true}, nil
	}

	if opts.SourceStatePath != "" {
		if err := copyFile(opts.SourceStatePath, monolithState); err != nil {
			return nil, fmt.Errorf("copy source state: %w", err)
		}
	} else {
		if err := pullState(ctx, srcDir, monolithState, opts); err != nil {
			return nil, fmt.Errorf("pull monolith state: %w", err)
		}
	}
	if err := copyFile(monolithState, backup); err != nil {
		return nil, fmt.Errorf("backup state: %w", err)
	}
	return &Prepared{MonolithState: monolithState, BackupPath: backup}, nil
}

// verifySameLineage compares the resumable working state's lineage against
// the current source state (the --state-file, or a read-only pull to a temp
// file). A mismatch means the workDir is stale — from another backend, or an
// earlier life of this root — and resuming would carve the wrong world.
func verifySameLineage(ctx context.Context, srcDir, monolithState, backupState string, opts Options) error {
	tmp, err := os.CreateTemp("", "demono-lineage-*.tfstate")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()

	if opts.SourceStatePath != "" {
		if err := copyFile(opts.SourceStatePath, tmpPath); err != nil {
			return err
		}
	} else if err := pullState(ctx, srcDir, tmpPath, opts); err != nil {
		return fmt.Errorf("pull state for resume check: %w", err)
	}

	srcLineage, err := stateLineage(tmpPath)
	if err != nil {
		return err
	}
	// The working copy's lineage matches the backup taken at carve start; use
	// the backup (the working copy legitimately diverges in serial as moves
	// are applied, but never in lineage).
	workLineage, err := stateLineage(backupState)
	if err != nil {
		workLineage, err = stateLineage(monolithState)
		if err != nil {
			return err
		}
	}
	if srcLineage == "" || srcLineage != workLineage {
		return fmt.Errorf("the working state in %s has a different lineage than the current source state — it is stale (an earlier backend or source?); delete the directory to carve fresh", filepath.Dir(monolithState))
	}
	return nil
}

// stateLineage reads the lineage of a state file.
func stateLineage(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var s struct {
		Lineage string `json:"lineage"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return "", fmt.Errorf("parse state %s: %w", path, err)
	}
	return s.Lineage, nil
}

// Carve executes the plan against local state copies and writes one state file
// per non-empty module into workDir. srcDir is the monolith root (used to pull
// state when SourceStatePath is empty).
func Carve(ctx context.Context, srcDir, workDir string, plan *Plan, opts Options) (*Result, error) {
	prep, err := Prepare(ctx, srcDir, workDir, opts)
	if err != nil {
		return nil, err
	}
	return Execute(ctx, srcDir, workDir, prep, plan, opts)
}

// Execute runs the plan's moves against the prepared working state.
func Execute(ctx context.Context, srcDir, workDir string, prep *Prepared, plan *Plan, opts Options) (*Result, error) {
	monolithState := prep.MonolithState
	res := &Result{ModuleStates: map[string]string{}, BackupPath: prep.BackupPath}

	// 3. A tfexec instance rooted at srcDir drives the moves; -state/-state-out
	//    make the operation entirely file-local.
	tf, err := newExec(srcDir, opts)
	if err != nil {
		return nil, err
	}

	modules := make([]string, 0, len(plan.Moves))
	for m := range plan.Moves {
		modules = append(modules, m)
	}
	sort.Strings(modules)

	for _, module := range modules {
		moves := plan.Moves[module]
		if len(moves) == 0 {
			res.Empty = append(res.Empty, module)
			continue
		}
		// The remainder module inherits whatever stays in the monolith state:
		// its resources are never moved (moving a resource to itself within one
		// file is an error), so the carved-down monolith file IS its state.
		if module == plan.Remainder {
			continue
		}
		destState := filepath.Join(workDir, module+".tfstate")
		for _, mv := range moves {
			// state mv -state=<monolith> -state-out=<module> src dest
			if err := tf.StateMv(ctx, mv.SourceAddr, mv.DestAddr,
				tfexec.State(monolithState),
				tfexec.StateOut(destState),
			); err != nil {
				return nil, fmt.Errorf("state mv %s -> module %s: %w", mv.SourceAddr, module, err)
			}
		}
		res.ModuleStates[module] = destState
	}

	// After carving out every non-remainder module, the monolith state holds
	// exactly the remainder module's resources. Adopt it as that module's state.
	if plan.AdoptRemainder {
		remState := filepath.Join(workDir, plan.Remainder+".tfstate")
		if remState != monolithState {
			if err := copyFile(monolithState, remState); err != nil {
				return nil, fmt.Errorf("adopt remainder state: %w", err)
			}
		}
		res.ModuleStates[plan.Remainder] = remState
	}

	return res, nil
}

// pullState runs `terraform state pull` in srcDir and writes the result to out.
func pullState(ctx context.Context, srcDir, out string, opts Options) error {
	tf, err := newExec(srcDir, opts)
	if err != nil {
		return err
	}
	if err := tf.Init(ctx); err != nil {
		return fmt.Errorf("init before pull: %w", err)
	}
	state, err := tf.StatePull(ctx)
	if err != nil {
		return err
	}
	return os.WriteFile(out, []byte(state), 0o600)
}

// newExec constructs a tfexec.Terraform rooted at dir with the selected engine.
func newExec(dir string, opts Options) (*tfexec.Terraform, error) {
	execPath := opts.ExecPath
	if execPath == "" {
		engine := opts.Engine
		if engine == "" {
			engine = EngineTerraform
		}
		p, err := lookPath(string(engine))
		if err != nil {
			return nil, err
		}
		execPath = p
	}
	return tfexec.NewTerraform(dir, execPath)
}
