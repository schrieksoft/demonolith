package manifest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/pipeline"
)

// analyzeFixture runs the pure pipeline over a testdata fixture's in/ dir.
func analyzeFixture(t *testing.T, fixture string) (*pipeline.Analysis, string) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "testdata", fixture, "in"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := pipeline.Analyze(dir, pipeline.Options{})
	if err != nil {
		t.Fatalf("analyze %s: %v", fixture, err)
	}
	return a, dir
}

func TestBuildWriteLoad_Roundtrip(t *testing.T) {
	a, srcDir := analyzeFixture(t, "statefix")
	rootDir := t.TempDir()
	outDir := filepath.Join(rootDir, "modules")

	created := time.Date(2026, 8, 15, 14, 30, 0, 0, time.UTC)
	m := BuildPlanned(a, rootDir, outDir, created, "demonolith test", BuildOpts{Bootstrap: false})
	if m.IsRun() {
		t.Fatal("a planned manifest must not be run")
	}

	e := &emit.Emitter{SrcDir: srcDir, OutDir: outDir, Graph: a.Graph, Place: a.Placement, Bound: a.Boundary}
	if _, err := e.Emit(); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := m.Finalize(rootDir, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if !m.IsRun() {
		t.Fatal("finalize must set the emit checksum")
	}
	if m.Version != SchemaVersion {
		t.Errorf("version = %d, want %d", m.Version, SchemaVersion)
	}
	if len(m.StateMoves) != 3 {
		t.Errorf("state moves = %d, want 3 (remainder resources must be absent)", len(m.StateMoves))
	}
	for _, mv := range m.StateMoves {
		if mv.Module == m.Source.RemainderModule {
			t.Errorf("state move %s targets the remainder; remainder resources must not move", mv.Address)
		}
	}
	if !m.RemainderIsStateful() {
		t.Error("statefix remainder holds random_pet.leftover; RemainderIsStateful should be true")
	}
	if got := m.Modules["a"].Dir; got != "modules/a" {
		t.Errorf("module dir stored as %q, want root-relative %q", got, "modules/a")
	}

	path := Path(rootDir)
	if filepath.Base(path) != "demonolith-refactor-map.yaml" {
		t.Errorf("unexpected manifest filename %s", filepath.Base(path))
	}
	if err := Write(m, path); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !SemanticEqual(m, got) {
		t.Error("roundtripped manifest is not semantically equal to the original")
	}
	if got.EmitChecksum != m.EmitChecksum {
		t.Errorf("checksum changed across roundtrip")
	}

	// The plan reconstructed from the manifest matches the moves and adopts the
	// remainder.
	plan := got.Plan()
	if !plan.AdoptRemainder {
		t.Error("reconstructed plan must adopt the remainder state")
	}
	if len(plan.Moves["a"]) != 2 || len(plan.Moves["b"]) != 1 {
		t.Errorf("reconstructed plan moves wrong: %+v", plan.Moves)
	}
}

func TestChecksum_IgnoresLaterStageArtifacts(t *testing.T) {
	dir := t.TempDir()
	mod := filepath.Join(dir, "a")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "main.tf"), []byte("resource \"x\" \"y\" {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirs := map[string]string{"a": mod}
	before, err := Checksum(dirs)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"terraform.tfstate", "demono.tfplan", "demono.root.tfvars", "demono.graph.tfvars", "demono.env", ".terraform.lock.hcl"} {
		if err := os.WriteFile(filepath.Join(mod, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(mod, ".terraform", "providers"), 0o755); err != nil {
		t.Fatal(err)
	}

	after, err := Checksum(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Error("checksum must ignore state/plan/tfvars/lock artifacts")
	}

	if err := os.WriteFile(filepath.Join(mod, "main.tf"), []byte("resource \"x\" \"z\" {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := Checksum(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if changed == before {
		t.Error("checksum must change when an emitted file changes")
	}
}

func TestLoad_RefusesFutureVersion(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir)
	if err := os.WriteFile(path, []byte("version: 99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("loading a future-versioned manifest must fail rather than guess")
	}
}

func TestReceipt_LatestFor(t *testing.T) {
	dir := t.TempDir()

	// The map receipt has one canonical file; a rewrite supersedes it.
	r1 := &Receipt{Version: 1, Manifest: FileName, ManifestChecksum: "sha256:aaa", Action: ActionMap, Complete: false}
	r2 := &Receipt{Version: 1, Manifest: FileName, ManifestChecksum: "sha256:aaa", Action: ActionMap, Complete: true}
	if _, err := WriteReceipt(r1, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteReceipt(r2, dir); err != nil {
		t.Fatal(err)
	}

	got, err := LatestReceiptFor(dir, "sha256:aaa", ActionMap)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.Complete {
		t.Errorf("the canonical receipt should be the superseding, complete one; got %+v", got)
	}
	// A different generation's checksum must not match the same file.
	none, err := LatestReceiptFor(dir, "sha256:ccc", ActionMap)
	if err != nil {
		t.Fatal(err)
	}
	if none != nil {
		t.Errorf("a receipt from a different manifest generation must not match; got %+v", none)
	}
	// Actions have separate canonical files: no run receipt exists yet.
	if r, _ := LatestReceiptFor(dir, "sha256:aaa", ActionRun); r != nil {
		t.Errorf("map receipt must not satisfy a run lookup; got %+v", r)
	}
}
