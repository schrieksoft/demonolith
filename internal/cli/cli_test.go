package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-exec/tfexec"

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
// finalizes, verify passes, and a source edit between plan and run is refused.
func TestRefactor_PlanThenRun(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-plan-run")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	if err := run(t, "refactor", "plan", "--root-dir", srcDir, "--out", "modules"); err != nil {
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
	if err := run(t, "migrate", "plan", "--root-dir", srcDir, "--exec-path", "/bin/true"); err == nil || !strings.Contains(err.Error(), "planned but not run") {
		t.Errorf("migrate plan should refuse a planned-only manifest, got: %v", err)
	}
	if err := run(t, "refactor", "verify", "--root-dir", srcDir, "--quiet"); ExitCode(err) != ExitVerdict {
		t.Errorf("verify of a planned-only manifest should be a verdict, got: %v", err)
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
	for _, mod := range []string{"a", "b", "monolith", "snapcd"} {
		if _, err := os.Stat(filepath.Join(srcDir, "modules", mod, "main.tf")); err != nil {
			t.Errorf("expected emitted root %s: %v", mod, err)
		}
	}
	if err := run(t, "refactor", "verify", "--root-dir", srcDir, "--quiet"); err != nil {
		t.Errorf("verify after run should pass: %v", err)
	}

	// Source drift between plan and run: re-plan, edit the source, run refuses.
	if err := run(t, "refactor", "plan", "--root-dir", srcDir, "--out", "modules"); err != nil {
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

// TestRefactor_BarePipeline: bare `refactor` = plan → run → verify, pausing
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

// TestRefactorVerify_Gate: verify passes on a clean tree, fails with a verdict
// after an emitted root is edited; --silent carries no message.
func TestRefactorVerify_Gate(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-verify-gate")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	mainTf := filepath.Join(srcDir, "modules", "a", "main.tf")
	b, _ := os.ReadFile(mainTf)
	if err := os.WriteFile(mainTf, append(b, []byte("\n# edited\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run(t, "refactor", "verify", "--root-dir", srcDir)
	if ExitCode(err) != ExitVerdict {
		t.Errorf("a difference should be a verdict (exit %d), got exit %d: %v", ExitVerdict, ExitCode(err), err)
	}
	err = run(t, "refactor", "verify", "--root-dir", srcDir, "--silent")
	if ExitCode(err) != ExitVerdict || (err != nil && err.Error() != "") {
		t.Errorf("--silent must keep the verdict exit code with no message, got: %v", err)
	}
}

// TestRefactorPlan_OutResolution: a relative --out resolves against
// --root-dir; an outside dir is refused as an operational error.
func TestRefactorPlan_OutResolution(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-out-resolution")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	if err := run(t, "refactor", "plan", "--root-dir", srcDir, "--out", "carved"); err != nil {
		t.Fatalf("plan with relative --out failed: %v", err)
	}
	m, err := manifest.Load(manifest.Path(srcDir))
	if err != nil {
		t.Fatal(err)
	}
	if m.Output.Dir != "carved" {
		t.Errorf("manifest output.dir = %q, want root-relative %q", m.Output.Dir, "carved")
	}
	err = run(t, "refactor", "plan", "--root-dir", srcDir, "--out", t.TempDir())
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
	if err := run(t, "refactor", "plan", "--root-dir", srcDir, "--out", "modules"); err == nil {
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
	err := run(t, "refactor", "plan", "--root-dir", srcDir, "--out", "modules")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("module name snapcd must be refused with bootstrap on, got: %v", err)
	}
	if err := run(t, "refactor", "plan", "--root-dir", srcDir, "--out", "modules", "--no-bootstrap"); err != nil {
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
	for _, mod := range []string{"a", "b", "monolith"} {
		st := filepath.Join(srcDir, "modules", mod, "terraform.tfstate")
		if _, err := os.Stat(st); err != nil {
			t.Errorf("module %s missing seeded state: %v", mod, err)
		}
	}
	planR, err := manifest.LatestReceiptFor(srcDir, m.EmitChecksum, manifest.ActionPlan)
	if err != nil || planR == nil || !planR.Complete {
		t.Fatalf("expected a complete plan receipt (err %v)", err)
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
	if err := run(t, "migrate", "plan", "--root-dir", srcDir, "--exec-path", execPath,
		"--state-file", filepath.Join(srcDir, "terraform.tfstate")); err != nil {
		t.Fatalf("migrate plan failed: %v", err)
	}
	err := run(t, "migrate", "run", "--root-dir", srcDir, "--exec-path", execPath)
	if err == nil || !strings.Contains(err.Error(), "prove") {
		t.Errorf("run without a verdict must refuse pointing at prove, got: %v", err)
	}
	if err := run(t, "migrate", "run", "--root-dir", srcDir, "--exec-path", execPath, "--unproven"); err != nil {
		t.Errorf("run --unproven should proceed: %v", err)
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
	if err == nil || !strings.Contains(err.Error(), "migrate plan") {
		t.Errorf("prove without a plan must refuse pointing at migrate plan, got: %v", err)
	}
}

// TestMigrateProve_CreateTfvars: tfvars materialization is opt-in; the files
// are the standalone wiring and stay in place.
func TestMigrateProve_CreateTfvars(t *testing.T) {
	execPath := testsupport.RequireEngine(t)

	base := testsupport.OutDir(t, "statefix", "cli-create-tfvars")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))
	testsupport.ApplyRoot(t, srcDir)

	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	if err := run(t, "migrate", "plan", "--root-dir", srcDir, "--exec-path", execPath,
		"--state-file", filepath.Join(srcDir, "terraform.tfstate")); err != nil {
		t.Fatalf("migrate plan failed: %v", err)
	}

	// Default: no tfvars files written.
	if err := run(t, "migrate", "prove", "--root-dir", srcDir, "--exec-path", execPath); err != nil {
		t.Fatalf("prove failed: %v", err)
	}
	bTfvars := filepath.Join(srcDir, "modules", "b", "generated.auto.tfvars")
	if _, serr := os.Stat(bTfvars); !os.IsNotExist(serr) {
		t.Error("prove must not write tfvars files by default")
	}

	// Opt-in: written and kept.
	if err := run(t, "migrate", "prove", "--root-dir", srcDir, "--exec-path", execPath, "--create-tfvars"); err != nil {
		t.Fatalf("prove --create-tfvars failed: %v", err)
	}
	b, err := os.ReadFile(bTfvars)
	if err != nil {
		t.Fatalf("expected generated.auto.tfvars for consumer b: %v", err)
	}
	if !strings.Contains(string(b), "random_integer_seed") {
		t.Errorf("b tfvars missing threaded input:\n%s", b)
	}
}

// TestMigratePlan_StaleManifest: editing an emitted root makes migrate plan
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
	err := run(t, "migrate", "plan", "--root-dir", srcDir, "--exec-path", "/bin/true")
	if ExitCode(err) != ExitVerdict {
		t.Errorf("staleness should be a verdict (exit %d), got exit %d: %v", ExitVerdict, ExitCode(err), err)
	}
}

// TestMigratePlan_RequiresEngine: without an engine, migrate plan refuses.
func TestMigratePlan_RequiresEngine(t *testing.T) {
	base := testsupport.OutDir(t, "statefix", "cli-migrate-engine")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("statefix"))

	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	if err := run(t, "migrate", "plan", "--root-dir", srcDir); err == nil {
		t.Fatal("migrate plan without --engine should fail")
	}
}

// TestRefactor_RefusesCycle asserts plan exits non-nil on an impossible split.
func TestRefactor_RefusesCycle(t *testing.T) {
	src := testsupport.InDir("cyclic")
	if err := run(t, "refactor", "plan", "--root-dir", src, "--out", "unused-out"); err == nil {
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
	for _, mod := range []string{"a", "b", "monolith"} {
		bt, err := os.ReadFile(filepath.Join(srcDir, "modules", mod, "backend.tf"))
		if err != nil {
			t.Fatalf("module %s missing backend.tf: %v", mod, err)
		}
		if !strings.Contains(string(bt), "monolith-"+mod+".tfstate") {
			t.Errorf("module %s backend.tf missing derived path:\n%s", mod, bt)
		}
	}

	if err := run(t, "migrate", "-y", "--root-dir", srcDir, "--exec-path", execPath); err != nil {
		t.Fatalf("bare migrate failed: %v", err)
	}
	for _, mod := range []string{"a", "b", "monolith"} {
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
	testsupport.ApplyRoot(t, srcDir)

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

	if err := run(t, "migrate", "-y", "--root-dir", srcDir, "--exec-path", execPath); err != nil {
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
}

// TestSampleJourney_SnapcdBackend is the same journey with the Snap CD State
// Store as the remote backend (seeded default/default credentials) and the
// shared configuration read from Snap CD data sources instead of a local
// file: exercises remote read-only pull, http backend-location derivation
// (lock endpoints included), and real remote pushes into empty state-store
// locations. Skips when no local Snap CD server is reachable.
func TestSampleJourney_SnapcdBackend(t *testing.T) {
	execPath := testsupport.RequireEngine(t)
	if !snapcdReachable() {
		t.Skip("no Snap CD server at localhost:5000")
	}

	base := testsupport.OutDir(t, "sample-snapcd", "cli-journey")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("sample-snapcd"))

	// Unique per-run state name: pushes must land in empty locations, and the
	// dev state store keeps old runs' states.
	stateName := fmt.Sprintf("demonolith-e2e-%d", time.Now().UnixNano())
	rootTf := filepath.Join(srcDir, "root.tf")
	b, err := os.ReadFile(rootTf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootTf, []byte(strings.ReplaceAll(string(b), "DEMONO_STATE_NAME", stateName)), 0o644); err != nil {
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
	if m.Backend == nil || m.Backend.Type != "http" {
		t.Fatalf("manifest should derive the http backend, got %+v", m.Backend)
	}
	wantLoc := "10000000-0000-0000-0000-000000000000/" + stateName + "-networking"
	if !strings.HasSuffix(m.Backend.Modules["networking"], wantLoc) {
		t.Errorf("derived location = %q, want suffix %q", m.Backend.Modules["networking"], wantLoc)
	}
	bt, err := os.ReadFile(filepath.Join(srcDir, "modules", "networking", "backend.tf"))
	if err != nil {
		t.Fatalf("networking backend.tf missing: %v", err)
	}
	if !strings.Contains(string(bt), stateName+"-networking/lock") {
		t.Errorf("lock endpoint must keep its verb segment after derivation:\n%s", bt)
	}

	// No config copies needed: the shared configuration lives in Snap CD.
	if err := run(t, "migrate", "-y", "--root-dir", srcDir, "--exec-path", execPath); err != nil {
		t.Fatalf("bare migrate failed: %v", err)
	}
	runR, err := manifest.LatestReceiptFor(srcDir, m.EmitChecksum, manifest.ActionRun)
	if err != nil || runR == nil || !runR.Complete {
		t.Fatalf("expected a complete run receipt (err %v)", err)
	}
	for _, p := range runR.Pushes {
		if p.Outcome != "pushed" {
			t.Errorf("module %s outcome = %q, want pushed", p.Module, p.Outcome)
		}
		if !strings.Contains(p.Location, stateName+"-"+p.Module) {
			t.Errorf("module %s pushed to %q, want the derived state-store location", p.Module, p.Location)
		}
	}
}

// snapcdReachable reports whether a local Snap CD server answers.
func snapcdReachable() bool {
	conn, err := net.DialTimeout("tcp", "localhost:5000", 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// copyTree recursively copies a directory tree.
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

// TestSampleJourney_BackendConfigFlags: the monolith's backend is configured
// entirely via -backend-config at init (empty HCL block). Plan falls back to
// the init-time resolved config for locations, refactor run persists the
// credentials as gitignored per-module .env files, and migrate sources them —
// the whole journey runs with zero backend flags. Skips without a server.
func TestSampleJourney_BackendConfigFlags(t *testing.T) {
	execPath := testsupport.RequireEngine(t)
	if !snapcdReachable() {
		t.Skip("no Snap CD server at localhost:5000")
	}

	base := testsupport.OutDir(t, "sample-snapcd", "cli-flags-only")
	srcDir := testsupport.CopyInto(t, filepath.Join(base, "src"), testsupport.InDir("sample-snapcd"))

	stateName := fmt.Sprintf("demonolith-e2e-flags-%d", time.Now().UnixNano())
	stateBase := "http://localhost:5000/api/10000000-0000-0000-0000-000000000000/state/10000000-0000-0000-0000-000000000000/" + stateName

	// Empty the backend block: everything arrives via -backend-config.
	rootTf := filepath.Join(srcDir, "root.tf")
	b, err := os.ReadFile(rootTf)
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	start := strings.Index(src, "backend \"http\" {")
	end := strings.Index(src[start:], "\n  }") + start + len("\n  }")
	src = src[:start] + "backend \"http\" {}" + src[end:]
	if err := os.WriteFile(rootTf, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	// Init + apply with the full config via flags.
	tf, err := tfexec.NewTerraform(srcDir, execPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err = tf.Init(ctx,
		tfexec.BackendConfig("address="+stateBase),
		tfexec.BackendConfig("lock_address="+stateBase+"/lock"),
		tfexec.BackendConfig("unlock_address="+stateBase+"/unlock"),
		tfexec.BackendConfig("lock_method=POST"),
		tfexec.BackendConfig("unlock_method=POST"),
		tfexec.BackendConfig("username=default"),
		tfexec.BackendConfig("password=default"),
	)
	if err != nil {
		t.Fatalf("init with -backend-config: %v", err)
	}
	if err := tf.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if err := run(t, "refactor", "-y", "--root-dir", srcDir, "--out", "modules"); err != nil {
		t.Fatalf("refactor failed: %v", err)
	}
	m, err := manifest.Load(manifest.Path(srcDir))
	if err != nil {
		t.Fatal(err)
	}
	if m.Backend == nil || !strings.HasSuffix(m.Backend.Modules["networking"], stateName+"-networking") {
		t.Fatalf("locations must derive from resolved config, got %+v", m.Backend)
	}
	// Refactor deals with code only: no credentials materialized yet.
	envPath := filepath.Join(srcDir, "modules", "networking", ".env")
	if _, serr := os.Stat(envPath); !os.IsNotExist(serr) {
		t.Error("refactor must not write .env files; that is migrate's job")
	}
	bt, _ := os.ReadFile(filepath.Join(srcDir, "modules", "networking", "backend.tf"))
	if strings.Contains(string(bt), "password") {
		t.Errorf("backend.tf must not carry credentials:\n%s", bt)
	}

	// The whole migration with zero backend flags: migrate run materializes
	// per-module .env from the root's resolved config and sources it.
	if err := run(t, "migrate", "-y", "--root-dir", srcDir, "--exec-path", execPath); err != nil {
		t.Fatalf("bare migrate failed: %v", err)
	}
	envB, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("expected a per-module .env after migrate run: %v", err)
	}
	if !strings.Contains(string(envB), "TF_HTTP_USERNAME=default") || !strings.Contains(string(envB), "TF_HTTP_PASSWORD=default") {
		t.Errorf(".env missing backend credentials:\n%s", envB)
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
