// Package proof is the graph-aware validation oracle: it plays Snap CD's role
// locally, walking modules in dependency order, threading each producer's
// planned output values into its consumers' inputs, and planning each module
// against a copy of its carved state. Zero changes everywhere - no create,
// destroy, or in-place update - proves the split operationally inert.
package proof

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"

	"github.com/schrieksoft/demonolith/internal/boundary"
	"github.com/schrieksoft/demonolith/internal/dotenv"
	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/statevars"
)

// Options configures a proof run.
type Options struct {
	ExecPath string
	// ExternalInputs supplies values for external/root inputs (former monolith
	// var.<name>), keyed by input name.
	ExternalInputs map[string]string
	// RootInputs seeds each module's variable values when no tfvars file was
	// materialized (--no-tfvars), keyed by module then variable name. Threaded
	// and external values still win over the seed.
	RootInputs map[string]map[string]string
	// OnPlanStart / OnPlanDone receive per-module notifications. Done is
	// always called ("plan FAILED" included), so a caller printing start
	// without a newline can complete the line either way.
	OnPlanStart func(module string)
	OnPlanDone  func(module, verdict string)
	// UseBackend plans each root against its configured real backend (full
	// init, no state staging) - migrate verify's mode.
	UseBackend bool
	// BackendConfig passes -backend-config values through to init when
	// UseBackend is set (out-of-band backend settings never stored in HCL).
	BackendConfig []string
}

// ModuleProof is the per-module result.
type ModuleProof struct {
	Module   string
	AddCount int
	Destroy  int
	Change   int // in-place updates
	ZeroDiff bool
	// Outputs are this module's output values extracted from the plan, keyed by
	// output name, threaded into downstream consumers.
	Outputs map[string]string
}

// Result is the whole proof bundle.
type Result struct {
	Order   []string
	Modules map[string]*ModuleProof
	// Threaded records, per consumer module, the cross-module input values
	// the proof threaded from producer plans - the values a materialized
	// graph tfvars cannot recover from state alone.
	Threaded map[string]map[string]string
	// OK is true iff every module planned to zero changes of any kind.
	OK bool
}

// Run executes the graph-threaded proof over the carved modules.
//
//   - moduleDirs: module name -> carved root directory (from emit).
//   - moduleStates: module name -> carved local state file (from statemove).
//   - bound: boundary result providing cross edges (the wiring to thread) and
//     input declarations.
func Run(ctx context.Context, moduleDirs, moduleStates map[string]string, bound *boundary.Result, opts Options) (*Result, error) {
	modules := make([]string, 0, len(moduleDirs))
	for m := range moduleDirs {
		modules = append(modules, m)
	}
	sort.Strings(modules)

	order, err := TopoOrder(modules, bound)
	if err != nil {
		return nil, err
	}

	// consumerInputs[module][inputName] = producing (module, outputName).
	type src struct{ module, output string }
	consumerInputs := map[string]map[string]src{}
	for _, e := range bound.CrossEdges {
		if consumerInputs[e.ConsumerModule] == nil {
			consumerInputs[e.ConsumerModule] = map[string]src{}
		}
		consumerInputs[e.ConsumerModule][e.InputName] = src{module: e.ProducerModule, output: e.OutputName}
	}

	planStart, planDone := opts.OnPlanStart, opts.OnPlanDone
	if planStart == nil {
		planStart = func(string) {}
	}
	if planDone == nil {
		planDone = func(string, string) {}
	}

	result := &Result{Order: order, Modules: map[string]*ModuleProof{}, Threaded: map[string]map[string]string{}, OK: true}
	extractedOutputs := map[string]map[string]string{} // module -> output -> value

	for _, module := range order {
		planStart(module)
		dir := moduleDirs[module]
		statePath := moduleStates[module]

		// Assemble input variable values for this module, seeded from the
		// in-memory root values when no tfvars file carries them.
		vars := map[string]string{}
		for k, v := range opts.RootInputs[module] {
			vars[k] = v
		}
		b := bound.Boundaries[module]
		if b != nil {
			for name, in := range b.Inputs {
				if in.External {
					// Only pass a value that exists: an empty -var
					// overrides the variable's in-module default.
					if v, ok := opts.ExternalInputs[name]; ok {
						vars[name] = v
					}
					continue
				}
				// Upstream-sourced: thread from the producer's extracted output.
				s := consumerInputs[module][name]
				pv, ok := extractedOutputs[s.module][s.output]
				if !ok {
					return nil, fmt.Errorf("module %q input %q: upstream output %q of module %q not available (topo/threading error)", module, name, s.output, s.module)
				}
				vars[name] = pv
				if result.Threaded[module] == nil {
					result.Threaded[module] = map[string]string{}
				}
				result.Threaded[module][name] = pv
			}
		}

		mp, outs, err := planModule(ctx, dir, statePath, vars, opts)
		if err != nil {
			planDone(module, "plan FAILED")
			return nil, fmt.Errorf("plan module %q: %w", module, err)
		}
		verdict := "zero changes"
		if !mp.ZeroDiff {
			verdict = fmt.Sprintf("CHANGES +%d ~%d -%d", mp.AddCount, mp.Change, mp.Destroy)
		}
		planDone(module, verdict)
		mp.Module = module
		mp.Outputs = outs
		result.Modules[module] = mp
		extractedOutputs[module] = outs
		if !mp.ZeroDiff {
			result.OK = false
		}
	}

	return result, nil
}

// planModule inits the carved root against a copy of its state, plans with the
// supplied input vars, asserts the change counts, and extracts output values.
func planModule(ctx context.Context, dir, statePath string, vars map[string]string, opts Options) (*ModuleProof, map[string]string, error) {
	tf, err := tfexec.NewTerraform(dir, opts.ExecPath)
	if err != nil {
		return nil, nil, err
	}
	if opts.UseBackend {
		// Source the module's demono.env (backend credentials) for the duration.
		env, err := dotenv.Load(filepath.Join(dir, emit.EnvFileName))
		if err != nil {
			return nil, nil, err
		}
		restore, err := dotenv.Apply(env)
		if err != nil {
			return nil, nil, err
		}
		defer restore()
		initOpts := []tfexec.InitOption{tfexec.Backend(true)}
		for _, bc := range opts.BackendConfig {
			initOpts = append(initOpts, tfexec.BackendConfig(bc))
		}
		if err := tf.Init(ctx, initOpts...); err != nil {
			return nil, nil, fmt.Errorf("init: %w", err)
		}
	} else {
		// Strip the backend from root.tf for the duration (required_providers
		// must stay): the proof judges code against carved state, and the
		// engine refuses to plan a declared-but-uninitialized backend.
		rootTF := filepath.Join(dir, "root.tf")
		if orig, rerr := os.ReadFile(rootTF); rerr == nil {
			if stripped, changed := emit.StripBackend(orig); changed {
				hold := rootTF + ".demono-hold"
				if err := os.WriteFile(hold, orig, 0o644); err != nil {
					return nil, nil, fmt.Errorf("hold root.tf: %w", err)
				}
				if err := os.WriteFile(rootTF, stripped, 0o644); err != nil {
					return nil, nil, fmt.Errorf("strip root.tf backend: %w", err)
				}
				defer func() { _ = os.Rename(hold, rootTF) }()
			}
		}
		// A hand-made root may keep its backend in a separate backend.tf; hold
		// that aside whole for the same reason.
		backendTF := filepath.Join(dir, "backend.tf")
		if _, err := os.Stat(backendTF); err == nil {
			hold := backendTF + ".demono-hold"
			if err := os.Rename(backendTF, hold); err != nil {
				return nil, nil, fmt.Errorf("hold backend.tf: %w", err)
			}
			defer func() { _ = os.Rename(hold, backendTF) }()
		}
		// A root previously init'd against its real backend records that in
		// .terraform/terraform.tfstate; with backend.tf held aside the engine
		// would demand a re-init. Hold the record aside for the same duration.
		initRecord := filepath.Join(dir, ".terraform", "terraform.tfstate")
		if _, err := os.Stat(initRecord); err == nil {
			hold := initRecord + ".demono-hold"
			if err := os.Rename(initRecord, hold); err != nil {
				return nil, nil, fmt.Errorf("hold backend init record: %w", err)
			}
			defer func() {
				_ = os.Remove(initRecord)
				_ = os.Rename(hold, initRecord)
			}()
		}
		// Stage the carved state at the default local location so it rules;
		// remove it afterwards and put back any pre-existing local state -
		// the proof reads state, it does not seed roots.
		if statePath != "" {
			localState := filepath.Join(dir, "terraform.tfstate")
			preserved := ""
			if _, err := os.Stat(localState); err == nil {
				preserved = localState + ".demono-preserve"
				if err := os.Rename(localState, preserved); err != nil {
					return nil, nil, fmt.Errorf("preserve existing state: %w", err)
				}
			}
			if err := copyFile(statePath, localState); err != nil {
				return nil, nil, fmt.Errorf("stage state: %w", err)
			}
			defer func() {
				_ = os.Remove(localState)
				if preserved != "" {
					_ = os.Rename(preserved, localState)
				}
			}()
		}
		if err := tf.Init(ctx, tfexec.Backend(false)); err != nil {
			return nil, nil, fmt.Errorf("init: %w", err)
		}
	}

	planPath := filepath.Join(dir, "demono.tfplan")
	// Never refresh: drift is out of scope - the proofs judge fidelity to the
	// pulled state, not the world. Data sources are the exception: plan reads
	// them live regardless, so their answers must hold still too.
	planOpts := []tfexec.PlanOption{
		tfexec.Out(planPath),
		tfexec.Refresh(false),
	}
	// The materialized tfvars files are loaded explicitly (they are not
	// .auto.tfvars), before the threaded -var values so threading wins.
	for _, name := range []string{statevars.RootTfvarsName, statevars.GraphTfvarsName} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			planOpts = append(planOpts, tfexec.VarFile(p))
		}
	}
	for k, v := range vars {
		planOpts = append(planOpts, tfexec.Var(fmt.Sprintf("%s=%s", k, v)))
	}
	if _, err := tf.Plan(ctx, planOpts...); err != nil {
		return nil, nil, fmt.Errorf("plan: %w", err)
	}

	plan, err := tf.ShowPlanFile(ctx, planPath)
	if err != nil {
		return nil, nil, fmt.Errorf("show plan: %w", err)
	}

	mp := countChanges(plan)
	outs := extractOutputs(plan)
	return mp, outs, nil
}

// countChanges tallies create/destroy/update actions from a plan.
func countChanges(plan *tfjson.Plan) *ModuleProof {
	mp := &ModuleProof{}
	for _, rc := range plan.ResourceChanges {
		if rc.Change == nil {
			continue
		}
		for _, a := range rc.Change.Actions {
			switch a {
			case tfjson.ActionCreate:
				mp.AddCount++
			case tfjson.ActionDelete:
				mp.Destroy++
			case tfjson.ActionUpdate:
				mp.Change++
			}
		}
	}
	// In-place updates fail too: a wrong input value that does not force a
	// replacement still shows up as an update, and tolerating it would let a
	// mis-wired split pass.
	mp.ZeroDiff = mp.AddCount == 0 && mp.Destroy == 0 && mp.Change == 0
	return mp
}

// extractOutputs pulls known output values from planned_values.outputs and
// stringifies them for threading into downstream -var inputs. Snap CD passes
// values as strings, so this matches its coercion.
func extractOutputs(plan *tfjson.Plan) map[string]string {
	out := map[string]string{}
	if plan.PlannedValues == nil || plan.PlannedValues.Outputs == nil {
		return out
	}
	for name, o := range plan.PlannedValues.Outputs {
		out[name] = stringify(o.Value)
	}
	return out
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

// PlanDir plans one root the way the proof plans a carved module - against a
// staged copy of statePath, or its real backend with opts.UseBackend -
// returning change counts and planned outputs. The transfer family's
// per-slice check.
func PlanDir(ctx context.Context, dir, statePath string, vars map[string]string, opts Options) (*ModuleProof, map[string]string, error) {
	return planModule(ctx, dir, statePath, vars, opts)
}
