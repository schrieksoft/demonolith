package proof_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/pipeline"
	"github.com/schrieksoft/demonolith/internal/proof"
	"github.com/schrieksoft/demonolith/internal/statemove"
	"github.com/schrieksoft/demonolith/internal/testsupport"
)

// carveStatefix runs apply -> analyze -> emit -> carve on the statefix monolith
// and returns everything proof.Run needs. Shared by the positive and negative
// proof tests.
func carveStatefix(t *testing.T, slug string) (analysis *pipeline.Analysis, moduleDirs, moduleStates map[string]string, execPath string) {
	t.Helper()
	execPath = testsupport.RequireEngine(t)
	ctx := context.Background()

	base := testsupport.OutDir(t, "statefix", slug)
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))
	testsupport.ApplyRoot(t, srcDir)

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
	moduleDirs = map[string]string{}
	for _, em := range ems {
		moduleDirs[em.Module] = em.Dir
	}

	workDir := filepath.Join(base, "state")
	plan := statemove.BuildPlan(a.Placement)
	carve, err := statemove.Carve(ctx, srcDir, workDir, plan, statemove.Options{
		ExecPath:        execPath,
		SourceStatePath: filepath.Join(srcDir, "terraform.tfstate"),
	})
	if err != nil {
		t.Fatalf("Carve: %v", err)
	}
	return a, moduleDirs, carve.ModuleStates, execPath
}

// corruptSeedResult rewrites random_integer.seed's stored `result` in a carved
// state file to newResult, so a plan of that module extracts a wrong output
// value. Used to prove the threaded proof catches a bad upstream value.
func corruptSeedResult(t *testing.T, statePath string, newResult int) {
	t.Helper()
	b, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read carved state: %v", err)
	}
	var st map[string]any
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("parse carved state: %v", err)
	}
	resources, _ := st["resources"].([]any)
	found := false
	for _, r := range resources {
		rm, _ := r.(map[string]any)
		if rm["type"] != "random_integer" || rm["name"] != "seed" {
			continue
		}
		insts, _ := rm["instances"].([]any)
		for _, in := range insts {
			im, _ := in.(map[string]any)
			attrs, _ := im["attributes"].(map[string]any)
			if attrs == nil {
				continue
			}
			attrs["result"] = newResult
			attrs["id"] = ""
			found = true
		}
	}
	if !found {
		t.Fatalf("random_integer.seed not found in carved state %s", statePath)
	}
	out, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(statePath, out, 0o600); err != nil {
		t.Fatalf("write corrupted state: %v", err)
	}
}

// TestProof_ZeroDiffWithThreading runs the full pipeline on the statefix
// monolith: apply -> carve state -> emit roots -> graph-threaded proof, and
// asserts every carved module plans to zero create/destroy with correctly
// threaded inter-module inputs.
func TestProof_ZeroDiffWithThreading(t *testing.T) {
	a, moduleDirs, moduleStates, execPath := carveStatefix(t, "zero-diff-threading")
	ctx := context.Background()

	res, err := proof.Run(ctx, moduleDirs, moduleStates, a.Boundary, proof.Options{
		ExecPath: execPath,
		Refresh:  false,
	})
	if err != nil {
		t.Fatalf("proof.Run: %v", err)
	}

	if !res.OK {
		for _, m := range res.Order {
			mp := res.Modules[m]
			t.Logf("module %s: +%d ~%d -%d zeroDiff=%v", m, mp.AddCount, mp.Change, mp.Destroy, mp.ZeroDiff)
		}
		t.Fatalf("proof not OK: at least one module has create/destroy")
	}

	// Producer 'a' must precede consumer 'b' in the proof order.
	posA, posB := indexOf(res.Order, "a"), indexOf(res.Order, "b")
	if posA < 0 || posB < 0 || posA > posB {
		t.Errorf("topo order should place a before b, got %v", res.Order)
	}

	// The threaded output must have carried a concrete seed value.
	aOut := res.Modules["a"].Outputs
	if _, ok := aOut["random_integer_seed"]; !ok {
		t.Errorf("module a should expose random_integer_seed output, got %v", aOut)
	}
}

// TestProof_CatchesMisthreadedValue proves the oracle actually validates the
// wiring, not just that each module plans in isolation. It carves the statefix
// monolith, then corrupts producer module "a"'s stored random_integer.seed
// result. When "a" is planned, it extracts the wrong seed as its output, which
// is threaded into consumer "b" whose random_pet.name_b keeps `seed` in its
// `keepers`. The mismatch against b's real state forces a replacement, so the
// proof must report OK == false. If the oracle didn't thread values, b would
// plan clean and this would wrongly pass.
func TestProof_CatchesMisthreadedValue(t *testing.T) {
	a, moduleDirs, moduleStates, execPath := carveStatefix(t, "catches-misthread")
	ctx := context.Background()

	// Sanity: with correct wiring the split is inert.
	good, err := proof.Run(ctx, moduleDirs, moduleStates, a.Boundary, proof.Options{ExecPath: execPath})
	if err != nil {
		t.Fatalf("baseline proof.Run: %v", err)
	}
	if !good.OK {
		t.Fatalf("baseline proof should be zero-diff, got OK=false")
	}

	// Corrupt a's stored seed so its extracted output no longer matches what b
	// was applied with. random_integer values are 1..100; 999 is guaranteed to
	// differ from the applied value.
	corruptSeedResult(t, moduleStates["a"], 999)

	bad, err := proof.Run(ctx, moduleDirs, moduleStates, a.Boundary, proof.Options{ExecPath: execPath})
	if err != nil {
		t.Fatalf("mis-thread proof.Run: %v", err)
	}
	if bad.OK {
		for _, m := range bad.Order {
			mp := bad.Modules[m]
			t.Logf("module %s: +%d ~%d -%d zeroDiff=%v outputs=%v", m, mp.AddCount, mp.Change, mp.Destroy, mp.ZeroDiff, mp.Outputs)
		}
		t.Fatal("proof should have caught the mis-threaded seed value, but reported OK")
	}

	// The failure must land on the consumer, not the producer.
	if mp := bad.Modules["b"]; mp.ZeroDiff {
		t.Errorf("consumer module b should show a diff from the wrong threaded seed, got %+v", mp)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
