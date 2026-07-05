package statemove_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/schrieksoft/demonolith/internal/pipeline"
	"github.com/schrieksoft/demonolith/internal/statemove"
	"github.com/schrieksoft/demonolith/internal/testsupport"
)

// resourcesInState reads a local state file and returns the set of
// "type.name" resource addresses it contains.
func resourcesInState(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state %s: %v", path, err)
	}
	var st struct {
		Resources []struct {
			Mode string `json:"mode"`
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("parse state %s: %v", path, err)
	}
	out := map[string]bool{}
	for _, r := range st.Resources {
		if r.Mode == "managed" {
			out[r.Type+"."+r.Name] = true
		}
	}
	return out
}

func TestCarve_SplitsStateByModule(t *testing.T) {
	execPath := testsupport.RequireEngine(t)

	// Apply the fixture to produce real local state.
	base := testsupport.OutDir(t, "statefix", "carve-splits-by-module")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))
	testsupport.ApplyRoot(t, srcDir)

	// Analyze placement from the same source.
	a, err := pipeline.Analyze(srcDir, pipeline.Options{Remainder: "monolith"})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	plan := statemove.BuildPlan(a.Placement)

	workDir := filepath.Join(base, "state")
	res, err := statemove.Carve(context.Background(), srcDir, workDir, plan, statemove.Options{
		ExecPath:        execPath,
		SourceStatePath: filepath.Join(srcDir, "terraform.tfstate"),
	})
	if err != nil {
		t.Fatalf("Carve: %v", err)
	}

	// Backup must exist.
	if _, err := os.Stat(res.BackupPath); err != nil {
		t.Errorf("backup missing: %v", err)
	}

	want := map[string]map[string]bool{
		"a":        {"random_integer.seed": true, "random_pet.name_a": true},
		"b":        {"random_pet.name_b": true},
		"monolith": {"random_pet.leftover": true},
	}
	for module, wantSet := range want {
		statePath, ok := res.ModuleStates[module]
		if !ok {
			t.Errorf("no carved state for module %q", module)
			continue
		}
		got := resourcesInState(t, statePath)
		if len(got) != len(wantSet) {
			t.Errorf("module %q state has %v, want %v", module, got, wantSet)
			continue
		}
		for addr := range wantSet {
			if !got[addr] {
				t.Errorf("module %q state missing %s (got %v)", module, addr, got)
			}
		}
	}

	// After carving out non-remainder modules, the monolith state retains
	// exactly the remainder module's resources (leftover), which it adopts.
	remaining := resourcesInState(t, filepath.Join(workDir, "monolith.tfstate"))
	if len(remaining) != 1 || !remaining["random_pet.leftover"] {
		t.Errorf("carved-down monolith state should hold only leftover, has %v", remaining)
	}
}
