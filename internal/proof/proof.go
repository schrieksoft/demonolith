// Package proof is the graph-aware validation oracle. Because
// snapcd_module_input_from_output is runtime metadata Terraform never sees, a
// carved module planned in isolation would have its upstream-sourced inputs
// unset. Demonolith plays Snap CD's role locally: it walks modules in
// dependency (topo) order, threads each producer's extracted output values into
// its consumers' inputs, and plans each module against a copy of its carved
// state. In the refactoring case the infrastructure already exists, so every
// output value is known at plan time and no apply is needed.
//
// The proof is the bundle of per-module plans that each show zero create/destroy
// against carved state with correctly threaded inputs — evidence that the split
// changes nothing operationally and the wiring is correct.
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
)

// Options configures a proof run.
type Options struct {
	ExecPath string
	// Refresh controls whether plan refreshes state against real providers.
	// false (default) is fast, credential-free, state-only — right for local
	// iteration. true gives an authoritative PR-proof run.
	Refresh bool
	// ExternalInputs supplies values for external/root inputs (former monolith
	// var.<name>), keyed by input name.
	ExternalInputs map[string]string
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
	// OK is true iff every module planned to zero create/destroy.
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

	order, err := topoOrder(modules, bound)
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

	result := &Result{Order: order, Modules: map[string]*ModuleProof{}, OK: true}
	extractedOutputs := map[string]map[string]string{} // module -> output -> value

	for _, module := range order {
		dir := moduleDirs[module]
		statePath := moduleStates[module]

		// Assemble input variable values for this module.
		vars := map[string]string{}
		b := bound.Boundaries[module]
		if b != nil {
			for name, in := range b.Inputs {
				if in.External {
					// Only pass a value when we actually have one; passing an
					// empty -var would override the variable's in-module default.
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
			}
		}

		mp, outs, err := planModule(ctx, dir, statePath, vars, opts)
		if err != nil {
			return nil, fmt.Errorf("plan module %q: %w", module, err)
		}
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
	// Place the carved state at the module's default local state location.
	localState := filepath.Join(dir, "terraform.tfstate")
	if statePath != "" {
		if err := copyFile(statePath, localState); err != nil {
			return nil, nil, fmt.Errorf("stage state: %w", err)
		}
	}

	tf, err := tfexec.NewTerraform(dir, opts.ExecPath)
	if err != nil {
		return nil, nil, err
	}
	if err := tf.Init(ctx); err != nil {
		return nil, nil, fmt.Errorf("init: %w", err)
	}

	planPath := filepath.Join(dir, "demono.tfplan")
	planOpts := []tfexec.PlanOption{
		tfexec.Out(planPath),
		tfexec.Refresh(opts.Refresh),
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
	mp.ZeroDiff = mp.AddCount == 0 && mp.Destroy == 0
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
