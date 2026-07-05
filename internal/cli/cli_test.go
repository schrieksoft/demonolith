package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schrieksoft/demonolith/internal/testsupport"
)

// TestSplit_EndToEnd drives the whole tool through the public command surface:
// it applies the statefix monolith, then runs `demono split <dir> --state
// --proof` via Root().Execute() and asserts the command succeeds, emits three
// carved roots, and carves a state file per module. This is the only test that
// exercises the CLI layer (flag wiring, runSplit, resolveExec, reporting) on top
// of the full analyze -> emit -> carve -> proof pipeline.
func TestSplit_EndToEnd(t *testing.T) {
	execPath := testsupport.RequireEngine(t)

	// Applied monolith with real local state.
	base := testsupport.OutDir(t, "statefix", "cli-end-to-end")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))
	testsupport.ApplyRoot(t, srcDir)

	outDir := filepath.Join(base, "modules")
	root := Root()
	root.SetArgs([]string{
		"split", srcDir,
		"--out", outDir,
		"--exec-path", execPath,
		"--state-file", filepath.Join(srcDir, "terraform.tfstate"),
		"--state",
		"--proof",
	})
	// Silence command output; assertions are on disk artifacts and exit status.
	root.SetOut(nil)
	root.SetErr(nil)

	if err := root.Execute(); err != nil {
		t.Fatalf("split --state --proof failed: %v", err)
	}

	// Three carved roots, each a directory with a main.tf.
	for _, module := range []string{"a", "b", "monolith"} {
		main := filepath.Join(outDir, module, "main.tf")
		if _, err := os.Stat(main); err != nil {
			t.Errorf("expected emitted root for module %q at %s: %v", module, main, err)
		}
	}

	// A carved state file per module under the .state work dir.
	stateDir := filepath.Join(outDir, ".state")
	for _, module := range []string{"a", "b", "monolith"} {
		st := filepath.Join(stateDir, module+".tfstate")
		if _, err := os.Stat(st); err != nil {
			t.Errorf("expected carved state for module %q at %s: %v", module, st, err)
		}
	}
}

// TestSplit_TfvarsFromState drives `split ... --tfvars --proof` on the applied
// e2e-split fixture through the command surface, asserting the command succeeds,
// generated.auto.tfvars lands in each consumer module, and the values come from
// the applied source state.
func TestSplit_TfvarsFromState(t *testing.T) {
	execPath := testsupport.RequireEngine(t)

	base := testsupport.OutDir(t, "e2e-split", "cli-tfvars-from-state")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("e2e-split"))
	testsupport.ApplyRoot(t, srcDir)

	outDir := filepath.Join(base, "modules")
	root := Root()
	root.SetArgs([]string{
		"split", srcDir,
		"--out", outDir,
		"--exec-path", execPath,
		"--state-file", filepath.Join(srcDir, "terraform.tfstate"),
		"--tfvars",
		"--proof",
	})
	root.SetOut(nil)
	root.SetErr(nil)

	if err := root.Execute(); err != nil {
		t.Fatalf("split --tfvars --proof failed: %v", err)
	}

	// Consumer modules app and network each have cross-module inputs, so each
	// must ship a generated.auto.tfvars populated from the applied state. (data
	// is a pure producer here — it consumes nothing cross-module.)
	for _, module := range []string{"app", "network"} {
		tfvars := filepath.Join(outDir, module, "generated.auto.tfvars")
		b, err := os.ReadFile(tfvars)
		if err != nil {
			t.Errorf("module %q missing generated.auto.tfvars: %v", module, err)
			continue
		}
		if strings.TrimSpace(string(b)) == "" {
			t.Errorf("module %q generated.auto.tfvars is empty", module)
		}
	}
	// app consumes net_id -> its tfvars must carry that value.
	appVars, _ := os.ReadFile(filepath.Join(outDir, "app", "generated.auto.tfvars"))
	if !strings.Contains(string(appVars), "random_integer_net_id") {
		t.Errorf("app tfvars missing net_id input:\n%s", appVars)
	}
}

// TestSplit_RefusesCycle asserts the command exits non-nil on an impossible
// split, without touching state or a binary (the cycle gate runs in the pure
// analyze half, before any I/O).
func TestSplit_RefusesCycle(t *testing.T) {
	src := testsupport.InDir("cyclic")
	outDir := testsupport.OutDir(t, "cyclic", "cli-refuses-cycle")

	root := Root()
	root.SetArgs([]string{"split", src, "--out", outDir})
	root.SetOut(nil)
	root.SetErr(nil)

	if err := root.Execute(); err == nil {
		t.Fatal("split of a cyclic monolith should fail, got nil error")
	}
}
