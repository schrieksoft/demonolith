package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schrieksoft/demonolith/internal/manifest"
	"github.com/schrieksoft/demonolith/internal/testsupport"
)

// run executes the root command with args and returns its error.
func run(t *testing.T, args ...string) error {
	t.Helper()
	root := Root()
	root.SetArgs(args)
	// Silence command output; assertions are on disk artifacts and exit status.
	root.SetOut(nil)
	root.SetErr(nil)
	return root.Execute()
}

// TestRefactorMigrateVerify_EndToEnd drives the full redesigned lifecycle
// through the public command surface: refactor emits roots and a manifest,
// migrate --dry-run needs no engine, migrate carves state and writes a
// receipt, a re-run skips via the receipt, and verify proves zero-diff in
// post-migrate mode and writes a verdict sidecar.
func TestRefactorMigrateVerify_EndToEnd(t *testing.T) {
	execPath := testsupport.RequireEngine(t)

	base := testsupport.OutDir(t, "statefix", "cli-end-to-end")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))
	testsupport.ApplyRoot(t, srcDir)

	outDir := filepath.Join(base, "modules")
	if err := run(t, "refactor", "--root-dir", srcDir, "--out", outDir); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}

	manifests, err := manifest.Discover(srcDir)
	if err != nil || len(manifests) != 1 {
		t.Fatalf("expected exactly one manifest in %s, got %v (err %v)", srcDir, manifests, err)
	}
	m, err := manifest.Load(manifests[0])
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if len(m.StateMoves) != 3 {
		t.Errorf("expected 3 state moves (a.seed, a.name_a, b.name_b), got %d: %+v", len(m.StateMoves), m.StateMoves)
	}
	if len(m.CrossEdges) != 1 {
		t.Errorf("expected 1 cross edge, got %d", len(m.CrossEdges))
	}

	for _, module := range []string{"a", "b", "monolith"} {
		main := filepath.Join(outDir, module, "main.tf")
		if _, err := os.Stat(main); err != nil {
			t.Errorf("expected emitted root for module %q at %s: %v", module, main, err)
		}
	}

	// Dry run needs no engine and must not create state artifacts.
	if err := run(t, "migrate", "--root-dir", srcDir, "--dry-run"); err != nil {
		t.Fatalf("migrate --dry-run failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, ".state")); !os.IsNotExist(err) {
		t.Errorf("dry run must not create the .state dir")
	}

	if err := run(t, "migrate", "--root-dir", srcDir, "--exec-path", execPath,
		"--state-file", filepath.Join(srcDir, "terraform.tfstate")); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	stateDir := filepath.Join(outDir, ".state")
	for _, module := range []string{"a", "b", "monolith"} {
		st := filepath.Join(stateDir, module+".tfstate")
		if _, err := os.Stat(st); err != nil {
			t.Errorf("expected carved state for module %q at %s: %v", module, st, err)
		}
	}
	receipt, err := manifest.LatestReceiptFor(srcDir, filepath.Base(manifests[0]))
	if err != nil || receipt == nil {
		t.Fatalf("expected a migrate receipt (err %v)", err)
	}
	if !receipt.Complete {
		t.Errorf("receipt should record a complete run")
	}

	// Idempotency: a second migrate skips via the receipt.
	if err := run(t, "migrate", "--root-dir", srcDir, "--exec-path", execPath); err != nil {
		t.Fatalf("re-run migrate should skip, got: %v", err)
	}

	// Resume: with the receipts gone, migrate finds the partially/fully carved
	// working state, detects every move as already applied, and completes
	// (writing a fresh receipt) instead of erroring.
	receipts, _ := filepath.Glob(filepath.Join(srcDir, manifest.ReceiptPrefix+"*.yaml"))
	for _, r := range receipts {
		if err := os.Remove(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(t, "migrate", "--root-dir", srcDir, "--exec-path", execPath); err != nil {
		t.Fatalf("resume after lost receipt should skip applied moves, got: %v", err)
	}
	receipt2, err := manifest.LatestReceiptFor(srcDir, filepath.Base(manifests[0]))
	if err != nil || receipt2 == nil || !receipt2.Complete {
		t.Fatalf("resume should write a fresh complete receipt (err %v, receipt %+v)", err, receipt2)
	}
	for _, mv := range receipt2.Moves {
		if mv.Outcome != "skipped" {
			t.Errorf("resumed move %s should be skipped (already applied), got %q", mv.Address, mv.Outcome)
		}
	}

	// Verify in post-migrate mode: proves the receipt's carved states.
	if err := run(t, "verify", "--root-dir", srcDir, "--exec-path", execPath); err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	verdicts, _ := filepath.Glob(filepath.Join(srcDir, manifest.VerdictPrefix+"*.yaml"))
	if len(verdicts) == 0 {
		t.Errorf("expected a verify verdict sidecar in %s", srcDir)
	}
}

// TestVerify_EphemeralAndTfvars proves a split before any migration has run:
// verify falls back to an ephemeral carve, and --keep-tfvars leaves the
// generated.auto.tfvars wiring in place with values from the applied state.
func TestVerify_EphemeralAndTfvars(t *testing.T) {
	execPath := testsupport.RequireEngine(t)

	base := testsupport.OutDir(t, "e2e-split", "cli-verify-ephemeral")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("e2e-split"))
	testsupport.ApplyRoot(t, srcDir)

	outDir := filepath.Join(base, "modules")
	if err := run(t, "refactor", "--root-dir", srcDir, "--out", outDir); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}

	// No migrate: verify must carve ephemerally and still prove zero-diff.
	if err := run(t, "verify", "--root-dir", srcDir, "--exec-path", execPath,
		"--state-file", filepath.Join(srcDir, "terraform.tfstate"), "--keep-tfvars"); err != nil {
		t.Fatalf("verify (ephemeral) failed: %v", err)
	}

	// The .state work dir must not exist: the ephemeral carve is a temp dir.
	if _, err := os.Stat(filepath.Join(outDir, ".state")); !os.IsNotExist(err) {
		t.Errorf("ephemeral verify must not write into the output dir's .state")
	}

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
	appVars, _ := os.ReadFile(filepath.Join(outDir, "app", "generated.auto.tfvars"))
	if !strings.Contains(string(appVars), "random_integer_net_id") {
		t.Errorf("app tfvars missing net_id input:\n%s", appVars)
	}
}

// TestRefactor_RefusesCycle asserts refactor exits non-nil on an impossible
// split, without touching state or a binary.
func TestRefactor_RefusesCycle(t *testing.T) {
	src := testsupport.InDir("cyclic")
	outDir := testsupport.OutDir(t, "cyclic", "cli-refuses-cycle")

	if err := run(t, "refactor", "--root-dir", src, "--out", outDir); err == nil {
		t.Fatal("refactor of a cyclic monolith should fail, got nil error")
	}
}

// TestRefactorCheck_DriftGate: a clean refactor passes --check; editing an
// emitted root then fails it with a verdict (exit 2) error.
func TestRefactorCheck_DriftGate(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-check-drift")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	outDir := filepath.Join(base, "modules")
	if err := run(t, "refactor", "--root-dir", srcDir, "--out", outDir); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	if err := run(t, "refactor", "--root-dir", srcDir, "--out", outDir, "--check"); err != nil {
		t.Fatalf("--check on a clean tree should pass: %v", err)
	}

	// Hand-edit an emitted root: --check must fail with a verdict.
	mainTf := filepath.Join(outDir, "a", "main.tf")
	b, err := os.ReadFile(mainTf)
	if err != nil {
		t.Fatalf("read emitted root: %v", err)
	}
	if err := os.WriteFile(mainTf, append(b, []byte("\n# edited\n")...), 0o644); err != nil {
		t.Fatalf("edit emitted root: %v", err)
	}
	err = run(t, "refactor", "--root-dir", srcDir, "--out", outDir, "--check")
	if err == nil {
		t.Fatal("--check after editing an emitted root should fail")
	}
	if ExitCode(err) != ExitVerdict {
		t.Errorf("drift should be a verdict (exit %d), got exit %d: %v", ExitVerdict, ExitCode(err), err)
	}
}

// TestMigrate_StaleManifest: editing an emitted root after refactor makes
// migrate refuse with a verdict.
func TestMigrate_StaleManifest(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-migrate-stale")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	outDir := filepath.Join(base, "modules")
	if err := run(t, "refactor", "--root-dir", srcDir, "--out", outDir); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	mainTf := filepath.Join(outDir, "a", "main.tf")
	b, _ := os.ReadFile(mainTf)
	if err := os.WriteFile(mainTf, append(b, []byte("\n# edited\n")...), 0o644); err != nil {
		t.Fatalf("edit emitted root: %v", err)
	}

	err := run(t, "migrate", "--root-dir", srcDir, "--dry-run")
	if err == nil {
		t.Fatal("migrate against a stale manifest should fail")
	}
	if ExitCode(err) != ExitVerdict {
		t.Errorf("staleness should be a verdict (exit %d), got exit %d: %v", ExitVerdict, ExitCode(err), err)
	}
}

// TestMigrate_RequiresEngine: without --dry-run, migrate refuses to run when
// neither --engine nor --exec-path is given.
func TestMigrate_RequiresEngine(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-migrate-engine")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	outDir := filepath.Join(base, "modules")
	if err := run(t, "refactor", "--root-dir", srcDir, "--out", outDir); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	if err := run(t, "migrate", "--root-dir", srcDir); err == nil {
		t.Fatal("migrate without --engine should fail")
	}
}
