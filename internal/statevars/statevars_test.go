package statevars_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"

	"github.com/schrieksoft/demonolith/internal/boundary"
	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/hclgraph"
	"github.com/schrieksoft/demonolith/internal/pipeline"
	"github.com/schrieksoft/demonolith/internal/statemove"
	"github.com/schrieksoft/demonolith/internal/statevars"
	"github.com/schrieksoft/demonolith/internal/testsupport"
)

// TestE2E_SplitProvenFromTfvars is the full end-to-end proof that a split is
// operationally inert when each carved module is fed values materialized from
// the applied source state.
//
// Prerequisite: the source root must be init'd and applied (a real
// terraform.tfstate exists). The test enforces this by applying the fixture
// itself.
//
// Unlike proof.Run (which threads producer plan outputs in memory), this test
// plans each module STANDALONE: only its carved state and its
// generated.auto.tfvars — no -var flags. That proves the .tfvars files alone
// carry every cross-module value correctly, which is what a real Snap CD deploy
// (or a human running `terraform plan` in the module dir) would see.
func TestE2E_SplitProvenFromTfvars(t *testing.T) {
	execPath := testsupport.RequireEngine(t)
	ctx := context.Background()

	// All artifacts land under testdata/e2e-split/out/<slug>/, wiped each run.
	base := testsupport.OutDir(t, "e2e-split", "proven-from-tfvars")

	// --- Prerequisite: an init'd + applied source root. -------------------
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("e2e-split"))
	testsupport.ApplyRoot(t, srcDir)
	sourceState := filepath.Join(srcDir, "terraform.tfstate")
	if _, err := os.Stat(sourceState); err != nil {
		t.Fatalf("prerequisite: applied source state missing: %v", err)
	}

	// --- Analyze + emit carved roots. -------------------------------------
	a, err := pipeline.Analyze(srcDir, pipeline.Options{Remainder: "monolith"})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	outDir := filepath.Join(base, "modules")
	e := &emit.Emitter{SrcDir: srcDir, OutDir: outDir, Graph: a.Graph, Place: a.Placement, Bound: a.Boundary}
	ems, err := e.Emit()
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	moduleDirs := map[string]string{}
	for _, em := range ems {
		moduleDirs[em.Module] = em.Dir
	}

	// Sanity: the fixture must actually exercise cross-module wiring, else the
	// test proves nothing.
	if len(a.Boundary.CrossEdges) < 2 {
		t.Fatalf("fixture must have >=2 cross-module edges to be a real e2e test, got %d", len(a.Boundary.CrossEdges))
	}

	// --- Cross-reference matrix: every consumer×producer kind combination. ---
	assertCrossRefMatrix(t, a.Boundary.CrossEdges)

	// --- Structural blocks (provider / locals / original variable) carved. ---
	assertStructuralCarving(t, moduleDirs)

	// --- Carve state into per-module local files. -------------------------
	workDir := filepath.Join(base, "state")
	plan := statemove.BuildPlan(a.Placement)
	carve, err := statemove.Carve(ctx, srcDir, workDir, plan, statemove.Options{
		ExecPath:        execPath,
		SourceStatePath: sourceState,
	})
	if err != nil {
		t.Fatalf("Carve: %v", err)
	}

	// --- Materialize .tfvars from the APPLIED source state. ---------------
	st, err := statevars.LoadState(sourceState)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	cross, err := statevars.ResolveCross(st, a.Boundary, statevars.Options{SourceDir: srcDir})
	if err != nil {
		t.Fatalf("ResolveCross: %v", err)
	}
	sv, err := statevars.Write(moduleDirs, nil, cross)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Every consumer module (one with >=1 cross-module input) must have a
	// tfvars file written.
	for _, b := range a.Boundary.Boundaries {
		hasUpstream := false
		for _, in := range b.Inputs {
			if !in.External {
				hasUpstream = true
			}
		}
		if hasUpstream {
			if _, ok := sv.Files[b.Module]; !ok {
				t.Errorf("module %q has upstream inputs but no generated.auto.tfvars", b.Module)
			}
		}
	}

	// --- Prove each module STANDALONE: carved state + .auto.tfvars only. ---
	for module, dir := range moduleDirs {
		// Stage the carved state at the module's default local path.
		if sp, ok := carve.ModuleStates[module]; ok {
			if err := copyFile(sp, filepath.Join(dir, "terraform.tfstate")); err != nil {
				t.Fatalf("stage state for %q: %v", module, err)
			}
		}
		add, destroy := planStandalone(t, ctx, execPath, dir)
		if add != 0 || destroy != 0 {
			t.Errorf("module %q is NOT inert: +%d create, -%d destroy (tfvars did not fully carry the split)", module, add, destroy)
		}
	}
}

// planStandalone inits and plans the module root with NO -var flags — it relies
// entirely on generated.auto.tfvars being auto-loaded. Returns create/destroy
// counts.
func planStandalone(t *testing.T, ctx context.Context, execPath, dir string) (add, destroy int) {
	t.Helper()
	tf, err := tfexec.NewTerraform(dir, execPath)
	if err != nil {
		t.Fatalf("new tf %q: %v", dir, err)
	}
	if err := tf.Init(ctx); err != nil {
		t.Fatalf("init %q: %v", dir, err)
	}
	planPath := filepath.Join(dir, "demono-e2e.tfplan")
	if _, err := tf.Plan(ctx, tfexec.Out(planPath), tfexec.Refresh(false)); err != nil {
		t.Fatalf("plan %q: %v", dir, err)
	}
	plan, err := tf.ShowPlanFile(ctx, planPath)
	if err != nil {
		t.Fatalf("show plan %q: %v", dir, err)
	}
	for _, rc := range plan.ResourceChanges {
		if rc.Change == nil {
			continue
		}
		for _, act := range rc.Change.Actions {
			switch act {
			case tfjson.ActionCreate:
				add++
			case tfjson.ActionDelete:
				destroy++
			}
		}
	}
	return add, destroy
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

// assertCrossRefMatrix verifies the fixture exercises every consumer×producer
// combination of cross-module reference. Producers: resource (R), module output
// (M), data source (D). Consumers: resource, module input, provider config,
// local. Each cell that the fixture is designed to cover must have >=1 edge.
func assertCrossRefMatrix(t *testing.T, edges []boundary.CrossEdge) {
	t.Helper()

	// consumerKind classifies the referencing side.
	consumerKind := func(a hclgraph.Address) string {
		switch a.Kind {
		case hclgraph.KindResource, hclgraph.KindData:
			return "resource"
		case hclgraph.KindModule:
			return "module-input"
		case hclgraph.KindProvider:
			return "provider"
		case hclgraph.KindLocal:
			return "local"
		}
		return "other"
	}
	// producerKind classifies the referenced side.
	producerKind := func(a hclgraph.Address) string {
		switch a.Kind {
		case hclgraph.KindResource:
			return "R"
		case hclgraph.KindModule:
			return "M"
		case hclgraph.KindData:
			return "D"
		}
		return "?"
	}

	seen := map[string]bool{}
	for _, e := range edges {
		seen[consumerKind(e.Consumer)+"→"+producerKind(e.Producer)] = true
	}

	// The combinations the fixture is built to cover (see testdata/e2e-split/in).
	// Notes on what's intentionally NOT a distinct edge:
	//   - D producers never cross a boundary: a data source follows its
	//     consumers, so every consumer reads a local copy.
	//   - provider→D is therefore impossible too.
	//   - local→M dedups into resource→M (same producer+input name); its carving
	//     is asserted separately via app's `local_from_module = var.module_idgen`.
	want := []string{
		"resource→R", "resource→M",
		"module-input→R",
		"provider→R", "provider→M",
		"local→R",
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("cross-ref matrix missing combination %q; edges present: %v", w, keysOf(seen))
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// assertStructuralCarving verifies that provider, locals, and original variable
// blocks were duplicated into exactly the modules that use them (and not into
// those that don't), for the e2e-split fixture. See testdata/e2e-split/in.
func assertStructuralCarving(t *testing.T, moduleDirs map[string]string) {
	t.Helper()

	read := func(module string) string {
		b, err := os.ReadFile(filepath.Join(moduleDirs[module], "main.tf"))
		if err != nil {
			t.Fatalf("read %s/main.tf: %v", module, err)
		}
		return string(b)
	}

	// want[module] = substrings that MUST appear; notWant = MUST NOT appear.
	type expect struct {
		want    []string
		notWant []string
	}
	cases := map[string]expect{
		// network owns module.idgen (carved with local source), uses name_prefix
		// (net_name.prefix) + common_length, and consumes random_string.token
		// cross-module (idgen.tag) -> var.random_string_token. No tls provider
		// (its resources are random-only).
		"network": {
			want: []string{
				`provider "random"`, `variable "name_prefix"`, "common_length",
				`module "idgen"`, `source = "./modules/idgen"`,
				"var.random_string_token",
			},
			notWant: []string{"tagged", `provider "tls"`},
		},
		// data owns tls_private_key.signer + data.tls_public_key.pub (the data
		// source follows its consumer fp_tag), so it carves the default tls
		// provider — whose config references var.name_prefix and
		// local.proxy_host, so those get pulled in too. Uses common_length; not
		// the app-only tagged.
		"data": {
			want:    []string{`provider "random"`, `provider "tls"`, "common_length", `variable "name_prefix"`, "proxy_host", `data "tls_public_key" "pub"`},
			notWant: []string{"tagged"},
		},
		// app uses THREE tls providers: default (var.name_prefix + local.proxy_host)
		// and two aliased ones referencing cross-module producers — by_resource
		// (net_name) and by_module (module.idgen). Plus name_prefix, common_length,
		// tagged, and locals rewritten to var.* for R/M producers.
		"app": {
			want: []string{
				`provider "random"`, `provider "tls"`,
				`alias = "by_resource"`, `alias = "by_module"`,
				`variable "name_prefix"`,
				"common_length", "tagged", "proxy_host",
				"var.module_idgen",
				"var.random_pet_net_name",
				"var.random_integer_net_id",
				"var.random_pet_fp_tag",
			},
			notWant: []string{`data "tls_public_key"`},
		},
		// catchall uses the random provider only; no locals/vars/tls.
		"monolith": {
			want:    []string{`provider "random"`},
			notWant: []string{"locals", `variable "name_prefix"`, `provider "tls"`},
		},
	}

	for module, exp := range cases {
		src := read(module)
		for _, w := range exp.want {
			if !strings.Contains(src, w) {
				t.Errorf("module %q main.tf missing %q:\n%s", module, w, src)
			}
		}
		for _, nw := range exp.notWant {
			if strings.Contains(src, nw) {
				t.Errorf("module %q main.tf should not contain %q:\n%s", module, nw, src)
			}
		}
	}

	// The child module's source directory must have been copied into network.
	childMain := filepath.Join(moduleDirs["network"], "modules", "idgen", "main.tf")
	if _, err := os.Stat(childMain); err != nil {
		t.Errorf("network module should carry copied child module source at %s: %v", childMain, err)
	}
}
