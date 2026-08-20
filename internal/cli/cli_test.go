package cli

import (
	"os"
	"path/filepath"
	"regexp"
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

// TestRefactor_PlanThenRun drives the plan/run split: plan writes only the
// manifest, downstream commands refuse a planned-only manifest, run emits and
// finalizes, diff passes, and a source edit between plan and run is refused.
func TestRefactor_PlanThenRun(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-plan-run")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	if err := run(t, "refactor", "map", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("plan failed: %v", err)
	}
	m, err := manifest.Load(manifest.Path(srcDir))
	if err != nil {
		t.Fatal(err)
	}
	if m.IsRun() {
		t.Error("a planned manifest must have no emit checksum")
	}
	if !m.Output.Bootstrap {
		t.Error("bootstrap intent should default on")
	}
	if _, serr := os.Stat(filepath.Join(srcDir, "modules")); !os.IsNotExist(serr) {
		t.Error("plan must emit nothing")
	}

	// Downstream refuses a planned-only manifest.
	if err := run(t, "migrate", "map", "--root-dir", srcDir, "--exec-path", "/bin/true"); err == nil || !strings.Contains(err.Error(), "has not been run") {
		t.Errorf("migrate map should refuse a planned-only manifest, got: %v", err)
	}
	if err := run(t, "refactor", "diff", "--root-dir", srcDir, "--quiet"); ExitCode(err) != ExitVerdict {
		t.Errorf("diff of a planned-only manifest should be a verdict, got: %v", err)
	}

	if err := run(t, "refactor", "run", "--root-dir", srcDir); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	m, err = manifest.Load(manifest.Path(srcDir))
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsRun() {
		t.Error("run must finalize the emit checksum")
	}
	for _, mod := range []string{"a", "b", "legacy", "snapcd"} {
		if _, err := os.Stat(filepath.Join(srcDir, "modules", mod, "main.tf")); err != nil {
			t.Errorf("expected emitted root %s: %v", mod, err)
		}
		gi, err := os.ReadFile(filepath.Join(srcDir, "modules", mod, ".gitignore"))
		if err != nil {
			t.Errorf("expected a .gitignore in emitted root %s: %v", mod, err)
		} else if !strings.Contains(string(gi), "demono.env") || !strings.Contains(string(gi), "*.tfstate") {
			t.Errorf("root %s .gitignore missing expected entries:\n%s", mod, gi)
		}
	}
	if err := run(t, "refactor", "diff", "--root-dir", srcDir, "--quiet"); err != nil {
		t.Errorf("diff after run should pass: %v", err)
	}

	// Source drift between plan and run: re-plan, edit the source, run refuses.
	if err := run(t, "refactor", "map", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("re-plan failed: %v", err)
	}
	mainTf := filepath.Join(srcDir, "main.tf")
	b, _ := os.ReadFile(mainTf)
	edited := append(b, []byte("\nresource \"random_pet\" \"sneaky\" {}\n")...)
	if err := os.WriteFile(mainTf, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	err = run(t, "refactor", "run", "--root-dir", srcDir)
	if ExitCode(err) != ExitVerdict {
		t.Errorf("run after a source edit must be a verdict, got %d: %v", ExitCode(err), err)
	}
	if err := os.WriteFile(mainTf, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRefactor_BarePipeline: bare `refactor` = map → run → diff, pausing
// for approval before run (-y approves; without a TTY it refuses).
func TestRefactor_BarePipeline(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-refactor-bare")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	err := run(t, "refactor", "--root-dir", srcDir, "--out", "modules")
	if err == nil || !strings.Contains(err.Error(), "-y") {
		t.Errorf("bare refactor without a TTY and without -y must refuse, got: %v", err)
	}

	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("bare refactor failed: %v", err)
	}
	m, err := manifest.Load(manifest.Path(srcDir))
	if err != nil || !m.IsRun() {
		t.Fatalf("bare refactor must leave a run manifest (err %v)", err)
	}
	if len(m.StateMoves) != 3 {
		t.Errorf("expected 3 state moves, got %d", len(m.StateMoves))
	}
	bs, err := os.ReadFile(filepath.Join(srcDir, "modules", "snapcd", "main.tf"))
	if err != nil {
		t.Fatalf("bootstrap missing: %v", err)
	}
	if !strings.Contains(string(bs), `resource "snapcd_module_input_from_output"`) {
		t.Error("bootstrap missing output wiring")
	}
}

// TestRefactorValidate: refuses without an engine and before run; the engine
// accepts a freshly written tree.
func TestRefactorValidate(t *testing.T) {
	execPath := testsupport.RequireEngine(t)
	base := testsupport.OutDir(t, "statefix", "cli-validate")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	if err := run(t, "refactor", "map", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("map failed: %v", err)
	}
	if err := run(t, "refactor", "validate", "--root-dir", srcDir, "--exec-path", execPath); err == nil || !strings.Contains(err.Error(), "has not been run") {
		t.Errorf("validate of a planned-only manifest should refuse, got: %v", err)
	}
	if err := run(t, "refactor", "run", "--root-dir", srcDir); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if err := run(t, "refactor", "validate", "--root-dir", srcDir); err == nil {
		t.Error("validate without an engine should fail")
	}
	if err := run(t, "refactor", "validate", "--root-dir", srcDir, "--exec-path", execPath); err != nil {
		t.Errorf("validate of a fresh tree should pass, got: %v", err)
	}
}

// TestRefactorDiff_Gate: diff passes on a clean tree, fails with a verdict
// after an emitted root is edited; --silent carries no message.
func TestRefactorDiff_Gate(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-diff-gate")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	mainTf := filepath.Join(srcDir, "modules", "a", "main.tf")
	b, _ := os.ReadFile(mainTf)
	if err := os.WriteFile(mainTf, append(b, []byte("\n# edited\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run(t, "refactor", "diff", "--root-dir", srcDir)
	if ExitCode(err) != ExitVerdict {
		t.Errorf("a difference should be a verdict (exit %d), got exit %d: %v", ExitVerdict, ExitCode(err), err)
	}
	err = run(t, "refactor", "diff", "--root-dir", srcDir, "--silent")
	if ExitCode(err) != ExitVerdict || (err != nil && err.Error() != "") {
		t.Errorf("--silent must keep the verdict exit code with no message, got: %v", err)
	}
}

// TestRefactorPlan_OutResolution: a relative --out resolves against
// --root-dir; an outside dir is refused as an operational error.
func TestRefactorPlan_OutResolution(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-out-resolution")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	if err := run(t, "refactor", "map", "--root-dir", srcDir, "--out", "carved"); err != nil {
		t.Fatalf("plan with relative --out failed: %v", err)
	}
	m, err := manifest.Load(manifest.Path(srcDir))
	if err != nil {
		t.Fatal(err)
	}
	if m.Output.Dir != "carved" {
		t.Errorf("manifest output.dir = %q, want root-relative %q", m.Output.Dir, "carved")
	}
	err = run(t, "refactor", "map", "--root-dir", srcDir, "--out", t.TempDir())
	if ExitCode(err) != ExitError {
		t.Errorf("outside --out is an operational error, got %d: %v", ExitCode(err), err)
	}
}

// TestRefactorPlan_RefusesForeignTargetDir: a target dir that exists and is
// not demonolith's own output fails the plan; nothing is written.
func TestRefactorPlan_RefusesForeignTargetDir(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-foreign-dir")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	foreign := filepath.Join(srcDir, "modules", "a")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "precious.tf"), []byte("# not demonolith's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(t, "refactor", "map", "--root-dir", srcDir, "--out", "modules"); err == nil {
		t.Fatal("plan into a foreign existing dir must fail")
	}
	if _, serr := os.Stat(manifest.Path(srcDir)); !os.IsNotExist(serr) {
		t.Error("refusal must not write a manifest")
	}

	if err := os.RemoveAll(foreign); err != nil {
		t.Fatal(err)
	}
	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("refactor after removing the collision failed: %v", err)
	}
	// Re-running over demonolith's own output stays allowed.
	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("re-running over own output must be allowed: %v", err)
	}
}

// TestRefactorPlan_ReservedBootstrapName: a module named snapcd is refused
// while bootstrap emission is on.
func TestRefactorPlan_ReservedBootstrapName(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-reserved-name")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))
	mainTf := filepath.Join(srcDir, "main.tf")
	b, _ := os.ReadFile(mainTf)
	edited := strings.Replace(string(b), "@demono:move a", "@demono:move snapcd", 1)
	if err := os.WriteFile(mainTf, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run(t, "refactor", "map", "--root-dir", srcDir, "--out", "modules")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("module name snapcd must be refused with bootstrap on, got: %v", err)
	}
	if err := run(t, "refactor", "map", "--root-dir", srcDir, "--out", "modules", "--no-bootstrap"); err != nil {
		t.Errorf("--no-bootstrap should free the name: %v", err)
	}
}

// TestMigrate_BarePipeline is the full state migration end to end on a local
// (backend-less) monolith: plan carves, prove passes over the artifacts, run
// seeds each root's local state, verify judges the result against reality.
func TestMigrate_BarePipeline(t *testing.T) {
	execPath := testsupport.RequireEngine(t)

	base := testsupport.OutDir(t, "statefix", "cli-migrate-bare")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))
	testsupport.ApplyRoot(t, srcDir)

	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	if err := run(t, "migrate", "-y", "--root-dir", srcDir, "--exec-path", execPath,
		"--state-file", filepath.Join(srcDir, "terraform.tfstate")); err != nil {
		t.Fatalf("bare migrate failed: %v", err)
	}

	m, err := manifest.Load(manifest.Path(srcDir))
	if err != nil {
		t.Fatal(err)
	}
	// Every root owns its state now (local mode: terraform.tfstate in place).
	for _, mod := range []string{"a", "b", "legacy"} {
		st := filepath.Join(srcDir, "modules", mod, "terraform.tfstate")
		if _, err := os.Stat(st); err != nil {
			t.Errorf("module %s missing seeded state: %v", mod, err)
		}
	}
	planR, err := manifest.LatestReceiptFor(srcDir, m.EmitChecksum, manifest.ActionMap)
	if err != nil || planR == nil || !planR.Complete {
		t.Fatalf("expected a complete map receipt (err %v)", err)
	}
	runR, err := manifest.LatestReceiptFor(srcDir, m.EmitChecksum, manifest.ActionRun)
	if err != nil || runR == nil || !runR.Complete {
		t.Fatalf("expected a complete run receipt (err %v)", err)
	}
	for _, p := range runR.Pushes {
		if p.Outcome != "pushed" {
			t.Errorf("module %s outcome = %q, want pushed", p.Module, p.Outcome)
		}
	}
	v, err := manifest.LatestProveVerdict(srcDir, m.EmitChecksum)
	if err != nil || v == nil || !v.OK {
		t.Fatalf("expected a passing prove verdict (err %v, v %+v)", err, v)
	}
	if _, err := os.Stat(filepath.Join(srcDir, manifest.FinalVerdictFile)); err != nil {
		t.Errorf("expected the final verdict sidecar: %v", err)
	}

	// Idempotent second pipeline: plan skips, run skips (same lineage).
	if err := run(t, "migrate", "-y", "--root-dir", srcDir, "--exec-path", execPath); err != nil {
		t.Fatalf("second bare migrate should be idempotent, got: %v", err)
	}
	runR2, err := manifest.LatestReceiptFor(srcDir, m.EmitChecksum, manifest.ActionRun)
	if err != nil || runR2 == nil {
		t.Fatal(err)
	}
	for _, p := range runR2.Pushes {
		if p.Outcome != "skipped" {
			t.Errorf("re-run module %s outcome = %q, want skipped", p.Module, p.Outcome)
		}
	}
}

// TestMigrateRun_RequiresProof: run refuses without a prove verdict and
// proceeds with --unproven.
func TestMigrateRun_RequiresProof(t *testing.T) {
	execPath := testsupport.RequireEngine(t)

	base := testsupport.OutDir(t, "statefix", "cli-run-guard")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))
	testsupport.ApplyRoot(t, srcDir)

	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	if err := run(t, "migrate", "map", "--root-dir", srcDir, "--exec-path", execPath,
		"--state-file", filepath.Join(srcDir, "terraform.tfstate")); err != nil {
		t.Fatalf("migrate map failed: %v", err)
	}
	err := run(t, "migrate", "run", "--root-dir", srcDir, "--exec-path", execPath)
	if err == nil || !strings.Contains(err.Error(), "prove") {
		t.Errorf("run without a verdict must refuse pointing at prove, got: %v", err)
	}

	// A target holding state that does not match the carve fails that module —
	// and the partial run receipt records how far the run got.
	unrelated := filepath.Join(srcDir, "modules", "b", "terraform.tfstate")
	if err := os.WriteFile(unrelated, []byte(`{"version":4,"lineage":"00000000-dead-beef-0000-000000000000","serial":9,"resources":[{"mode":"managed","type":"random_pet","name":"foreign"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err = run(t, "migrate", "run", "--root-dir", srcDir, "--exec-path", execPath, "--unproven")
	if err == nil || !strings.Contains(err.Error(), "module b") {
		t.Fatalf("run against a mismatched target must fail on that module, got: %v", err)
	}
	partial, err := manifest.LoadReceipt(filepath.Join(srcDir, manifest.RunReceiptFile))
	if err != nil {
		t.Fatalf("a failed run must leave a partial receipt: %v", err)
	}
	if partial.Complete || len(partial.Pushes) != 1 {
		t.Errorf("partial receipt should record the 1 completed push and not be complete, got complete=%v pushes=%d", partial.Complete, len(partial.Pushes))
	}

	// --overwrite replaces the mismatched occupant; matching targets still skip.
	if err := run(t, "migrate", "run", "--root-dir", srcDir, "--exec-path", execPath, "--unproven", "--overwrite"); err != nil {
		t.Errorf("retry with --overwrite should proceed: %v", err)
	}
	overwritten, err := manifest.LoadReceipt(filepath.Join(srcDir, manifest.RunReceiptFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range overwritten.Pushes {
		if p.Module == "b" && p.Outcome != "overwritten" {
			t.Errorf("module b with --overwrite: outcome %q, want overwritten", p.Outcome)
		}
		if p.Module == "a" && p.Outcome != "skipped" {
			t.Errorf("module a with --overwrite: outcome %q, want skipped", p.Outcome)
		}
	}

	// Retry after a re-carve: the fresh carve mints new lineages, but targets
	// holding identical content are an idempotent skip, not a refusal.
	if err := os.RemoveAll(filepath.Join(srcDir, "modules", ".demono")); err != nil {
		t.Fatal(err)
	}
	if err := run(t, "migrate", "map", "--root-dir", srcDir, "--exec-path", execPath,
		"--state-file", filepath.Join(srcDir, "terraform.tfstate")); err != nil {
		t.Fatalf("re-plan failed: %v", err)
	}
	if err := run(t, "migrate", "run", "--root-dir", srcDir, "--exec-path", execPath, "--unproven"); err != nil {
		t.Errorf("re-run after a re-carve must skip identically-seeded targets: %v", err)
	}
	final, err := manifest.LoadReceipt(filepath.Join(srcDir, manifest.RunReceiptFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range final.Pushes {
		if p.Outcome != "skipped" {
			t.Errorf("module %s after re-carve retry: outcome %q, want skipped", p.Module, p.Outcome)
		}
	}
}

// TestMigrateProve_RequiresPlan: prove judges plan's output and refuses when
// there is none.
func TestMigrateProve_RequiresPlan(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-prove-guard")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	err := run(t, "migrate", "prove", "--root-dir", srcDir, "--exec-path", "/bin/true")
	if err == nil || !strings.Contains(err.Error(), "migrate map") {
		t.Errorf("prove without a plan must refuse pointing at migrate map, got: %v", err)
	}
}

// TestMigrateProve_Tfvars: prove materializes demono.root.tfvars (this
// fixture has no root values, so no file), run materializes
// demono.graph.tfvars with the cross-module values, and --no-tfvars writes
// nothing while everything still threads in memory.
func TestMigrateProve_Tfvars(t *testing.T) {
	execPath := testsupport.RequireEngine(t)

	base := testsupport.OutDir(t, "statefix", "cli-tfvars")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))
	testsupport.ApplyRoot(t, srcDir)

	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	if err := run(t, "migrate", "map", "--root-dir", srcDir, "--exec-path", execPath,
		"--state-file", filepath.Join(srcDir, "terraform.tfstate")); err != nil {
		t.Fatalf("migrate map failed: %v", err)
	}

	// Prove writes root values only — graph values stay threaded in memory.
	if err := run(t, "migrate", "prove", "--root-dir", srcDir, "--exec-path", execPath); err != nil {
		t.Fatalf("prove failed: %v", err)
	}
	bGraph := filepath.Join(srcDir, "modules", "b", "demono.graph.tfvars")
	if _, serr := os.Stat(bGraph); !os.IsNotExist(serr) {
		t.Error("prove must not materialize graph values; that is run's job")
	}

	// Run materializes the graph file.
	if err := run(t, "migrate", "run", "--root-dir", srcDir, "--exec-path", execPath); err != nil {
		t.Fatalf("migrate run failed: %v", err)
	}
	b, err := os.ReadFile(bGraph)
	if err != nil {
		t.Fatalf("expected demono.graph.tfvars for consumer b after run: %v", err)
	}
	if !strings.Contains(string(b), "random_integer_seed") {
		t.Errorf("b graph tfvars missing cross-module input:\n%s", b)
	}

	// --no-tfvars: nothing written; the re-run skips seeded targets anyway.
	if err := os.Remove(bGraph); err != nil {
		t.Fatal(err)
	}
	if err := run(t, "migrate", "run", "--root-dir", srcDir, "--exec-path", execPath, "--no-tfvars"); err != nil {
		t.Fatalf("run --no-tfvars failed: %v", err)
	}
	if _, serr := os.Stat(bGraph); !os.IsNotExist(serr) {
		t.Error("run --no-tfvars must not materialize tfvars files")
	}
}

// TestMigratePlan_StaleManifest: editing an emitted root makes migrate map
// refuse with a verdict.
func TestMigratePlan_StaleManifest(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-migrate-stale")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	mainTf := filepath.Join(srcDir, "modules", "a", "main.tf")
	b, _ := os.ReadFile(mainTf)
	if err := os.WriteFile(mainTf, append(b, []byte("\n# edited\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run(t, "migrate", "map", "--root-dir", srcDir, "--exec-path", "/bin/true")
	if ExitCode(err) != ExitVerdict {
		t.Errorf("staleness should be a verdict (exit %d), got exit %d: %v", ExitVerdict, ExitCode(err), err)
	}
}

// TestMigratePlan_RequiresEngine: without an engine, migrate map refuses.
func TestMigratePlan_RequiresEngine(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-migrate-engine")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	if err := run(t, "migrate", "map", "--root-dir", srcDir); err == nil {
		t.Fatal("migrate map without --engine should fail")
	}
}

// TestRefactor_RefusesCycle asserts plan exits non-nil on an impossible split.
func TestRefactor_RefusesCycle(t *testing.T) {
	src := testsupport.InDir("cyclic")
	if err := run(t, "refactor", "map", "--root-dir", src, "--out", "unused-out"); err == nil {
		t.Fatal("plan of a cyclic monolith should fail, got nil error")
	}
}

// TestMigrate_DeclaredBackend drives the pipeline over a monolith with a
// declared (local-type) backend: plan derives per-module locations, run seeds
// them through the engine (init + empty-check + state push), verify judges
// against the real backends.
func TestMigrate_DeclaredBackend(t *testing.T) {
	execPath := testsupport.RequireEngine(t)

	base := testsupport.OutDir(t, "statefix", "cli-declared-backend")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))
	if err := os.WriteFile(filepath.Join(srcDir, "backend.tf"),
		[]byte("terraform {\n  backend \"local\" {\n    path = \"monolith.tfstate\"\n  }\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testsupport.ApplyRoot(t, srcDir)

	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	m, err := manifest.Load(manifest.Path(srcDir))
	if err != nil {
		t.Fatal(err)
	}
	if m.Backend == nil || m.Backend.Type != "local" {
		t.Fatalf("manifest should carry the derived local backend, got %+v", m.Backend)
	}
	if m.Backend.Modules["a"] != "monolith-a.tfstate" {
		t.Errorf("derived location = %q, want monolith-a.tfstate", m.Backend.Modules["a"])
	}
	for _, mod := range []string{"a", "b", "legacy"} {
		bt, err := os.ReadFile(filepath.Join(srcDir, "modules", mod, "root.tf"))
		if err != nil {
			t.Fatalf("module %s missing root.tf: %v", mod, err)
		}
		if !strings.Contains(string(bt), "monolith-"+mod+".tfstate") {
			t.Errorf("module %s root.tf missing derived path:\n%s", mod, bt)
		}
	}

	if err := run(t, "migrate", "-y", "--root-dir", srcDir, "--exec-path", execPath); err != nil {
		t.Fatalf("bare migrate failed: %v", err)
	}
	for _, mod := range []string{"a", "b", "legacy"} {
		st := filepath.Join(srcDir, "modules", mod, "monolith-"+mod+".tfstate")
		if _, err := os.Stat(st); err != nil {
			t.Errorf("module %s missing pushed backend state: %v", mod, err)
		}
	}
	runR, err := manifest.LatestReceiptFor(srcDir, m.EmitChecksum, manifest.ActionRun)
	if err != nil || runR == nil || !runR.Complete {
		t.Fatalf("expected a complete run receipt (err %v)", err)
	}
	for _, p := range runR.Pushes {
		if p.Outcome != "pushed" {
			t.Errorf("module %s outcome = %q, want pushed", p.Module, p.Outcome)
		}
	}
}

// TestSampleJourney_Local runs the full journey over the realistic showcase
// monolith (local child modules, GitHub modules, multi-consumer data sources,
// a config-file path dependency): refactor pipeline, the documented
// config-copy handling, then the bare migrate pipeline to zero-diff against
// the seeded local states.
func TestSampleJourney_Local(t *testing.T) {
	execPath := testsupport.RequireEngine(t)

	base := testsupport.OutDir(t, "sample", "cli-journey")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("sample"))

	// Vars arrive through all three channels, each the sole source of its
	// value: name_prefix from the root's terraform.tfvars,
	// resource_group_name from TF_VAR_* env, database_port from a -var flag
	// (tfvars files outrank env in the engine, so those two are stripped from
	// the copied tfvars). The env var stays set for the whole journey (the
	// documented same-shell requirement); the -var-only value is deliberately
	// absent from env after apply and must be re-supplied to migrate as --var.
	tfvPath := filepath.Join(srcDir, "terraform.tfvars")
	tfvB, err := os.ReadFile(tfvPath)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(tfvB), "\n") {
		if strings.HasPrefix(line, "resource_group_name") || strings.HasPrefix(line, "database_port") {
			continue
		}
		kept = append(kept, line)
	}
	if err := os.WriteFile(tfvPath, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF_VAR_resource_group_name", "env-rg")
	t.Setenv("TF_VAR_database_port", "7777")
	testsupport.ApplyRoot(t, srcDir)
	os.Unsetenv("TF_VAR_database_port")

	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	// The documented path-wrinkle handling: copy the config into each carved
	// root that reads it, then re-run so the checksum covers the copies.
	for _, mod := range []string{"networking", "database", "cluster", "app"} {
		if err := copyTree(filepath.Join(srcDir, "config"), filepath.Join(srcDir, "modules", mod, "config")); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(t, "refactor", "run", "--root-dir", srcDir); err != nil {
		t.Fatalf("refactor run after config copies failed: %v", err)
	}

	if err := run(t, "migrate", "-y", "--root-dir", srcDir, "--exec-path", execPath, "--var", "database_port=7777"); err != nil {
		t.Fatalf("bare migrate failed: %v", err)
	}
	m, err := manifest.Load(manifest.Path(srcDir))
	if err != nil {
		t.Fatal(err)
	}
	runR, err := manifest.LatestReceiptFor(srcDir, m.EmitChecksum, manifest.ActionRun)
	if err != nil || runR == nil || !runR.Complete {
		t.Fatalf("expected a complete run receipt (err %v)", err)
	}
	if len(runR.Pushes) != 5 {
		t.Errorf("expected 5 pushes, got %d", len(runR.Pushes))
	}
	v, err := manifest.LatestProveVerdict(srcDir, m.EmitChecksum)
	if err != nil || v == nil || !v.OK {
		t.Fatalf("expected a passing prove verdict (err %v)", err)
	}
	// All three channels land in the module's demono.root.tfvars; demono.env
	// is backend credentials only, so a backend-less monolith gets none.
	tfv, err := os.ReadFile(filepath.Join(srcDir, "modules", "database", "demono.root.tfvars"))
	if err != nil {
		t.Fatalf("expected a demono.root.tfvars after migrate: %v", err)
	}
	for name, val := range map[string]string{"name_prefix": "acme", "resource_group_name": "env-rg", "database_port": "7777"} {
		if !hasAssignment(string(tfv), name, val) {
			t.Errorf("demono.root.tfvars missing %s = %q:\n%s", name, val, tfv)
		}
	}
	if _, serr := os.Stat(filepath.Join(srcDir, "modules", "networking", "demono.env")); !os.IsNotExist(serr) {
		t.Error("demono.env is backend-credentials only; a backend-less monolith must get none")
	}
	// Cross values state cannot resolve (child-module outputs, expressions
	// included) are filled into the graph tfvars from the proof's threaded
	// planned outputs.
	gtv, err := os.ReadFile(filepath.Join(srcDir, "modules", "app", "demono.graph.tfvars"))
	if err != nil {
		t.Fatalf("expected a demono.graph.tfvars for app after migrate: %v", err)
	}
	for _, name := range []string{"module_cluster_cluster_endpoint", "module_cluster_cluster_id", "module_database"} {
		if !strings.Contains(string(gtv), name+" ") {
			t.Errorf("app graph tfvars missing proof-filled input %s:\n%s", name, gtv)
		}
	}
}

// hasAssignment reports whether content carries `name = "val"`, tolerating
// hclwrite's column-aligned padding around the equals sign.
func hasAssignment(content, name, val string) bool {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*=\s*"` + regexp.QuoteMeta(val) + `"`)
	return re.MatchString(content)
}

// copyTree copies a directory tree verbatim.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode())
	})
}

// TestRefactorDiff_Monorepo: monorepo-mode relative module sources must
// diff cleanly even though diff re-emits into a scratch dir — the paths
// are computed against the real out dir, not the physical emit dir.
func TestRefactorDiff_Monorepo(t *testing.T) {
	base := testsupport.OutDir(t, "sample", "cli-diff-monorepo")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("sample"))

	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--monorepo"); err != nil {
		t.Fatalf("monorepo refactor failed: %v", err)
	}
	if err := run(t, "refactor", "diff", "--root-dir", srcDir, "--quiet"); err != nil {
		t.Errorf("monorepo diff should pass: %v", err)
	}
}
