package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schrieksoft/demonolith/internal/testsupport"
	"github.com/schrieksoft/demonolith/internal/transfer"
)

// copyState copies a fixture's seed terraform.tfstate into a working copy
// (CopyInto deliberately skips state files).
func copyState(t *testing.T, fixtureDir, dst string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, "terraform.tfstate"))
	if err != nil {
		t.Fatalf("read seed state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "terraform.tfstate"), b, 0o644); err != nil {
		t.Fatalf("write seed state: %v", err)
	}
}

func stateResources(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "terraform.tfstate"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var raw struct {
		Serial    int `json:"serial"`
		Resources []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	var out []string
	for _, r := range raw.Resources {
		out = append(out, r.Type+"."+r.Name)
	}
	return out
}

// TestTransfer_EndToEnd drives the whole transfer family against two local
// roots: map pins both states, code moves the blocks, prove shows both plan
// clean, run rewrites both states (receiver first), verify replans for real,
// and a second run is a no-op.
func TestTransfer_EndToEnd(t *testing.T) {
	execPath := testsupport.RequireEngine(t)
	base := testsupport.OutDir(t, "transfer", "e2e")
	source := testsupport.CopyInto(t, filepath.Join(base, "source"), filepath.Join(testsupport.InDir("transfer"), "source"))
	recv := testsupport.CopyInto(t, filepath.Join(base, "shared"), filepath.Join(testsupport.InDir("transfer"), "shared"))
	copyState(t, filepath.Join(testsupport.InDir("transfer"), "source"), source)
	copyState(t, filepath.Join(testsupport.InDir("transfer"), "shared"), recv)

	// refactor map (pure, offline)
	if err := run(t, "transfer", "refactor", "map", "--root-dir", source); err != nil {
		t.Fatalf("transfer refactor map: %v", err)
	}
	m, err := transfer.LoadMap(source)
	if err != nil {
		t.Fatalf("load map: %v", err)
	}
	r := m.Receivers["../shared"]
	if len(r.Moves) != 2 || len(r.Blocks) != 2 {
		t.Fatalf("map should record 2 moved blocks, got %+v", r)
	}
	if len(r.Variables) != 1 || r.Variables[0] != "pet_length" || len(r.Locals) != 1 {
		t.Fatalf("map should record carried structural decls, got vars=%v locals=%v", r.Variables, r.Locals)
	}

	// migrate half before the code move refuses
	if err := run(t, "transfer", "migrate", "map", "--root-dir", source, "--exec-path", execPath); err == nil || !strings.Contains(err.Error(), "has not been run") {
		t.Fatalf("migrate before refactor run must refuse, got: %v", err)
	}

	// refactor run (the code move)
	if err := run(t, "transfer", "refactor", "run", "--root-dir", source); err != nil {
		t.Fatalf("transfer refactor run: %v", err)
	}
	sourceMain, err := os.ReadFile(filepath.Join(source, "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sourceMain), "move_me") || strings.Contains(string(sourceMain), "@demono:move") {
		t.Fatalf("source still contains moved blocks or decorators:\n%s", sourceMain)
	}
	recvMain, err := os.ReadFile(filepath.Join(recv, "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`resource "random_pet" "existing"`, `resource "random_pet" "move_me"`, `resource "random_integer" "move_too"`, "pet_len"} {
		if !strings.Contains(string(recvMain), want) {
			t.Fatalf("receiver main.tf missing %q:\n%s", want, recvMain)
		}
	}
	recvVars, err := os.ReadFile(filepath.Join(recv, "variables.tf"))
	if err != nil || !strings.Contains(string(recvVars), `variable "pet_length"`) {
		t.Fatalf("receiver variables.tf: %v\n%s", err, recvVars)
	}

	// refactor diff gate: per slice from each root, then the whole check
	if err := run(t, "transfer", "refactor", "diff", "--root-dir", recv); err != nil {
		t.Fatalf("transfer refactor diff (receiver slice): %v", err)
	}
	if err := run(t, "transfer", "refactor", "diff", "--all", "--root-dir", source); err != nil {
		t.Fatalf("transfer refactor diff: %v", err)
	}

	// migrate map (pull, pin, apply moves to local copies), then prove
	if err := run(t, "transfer", "migrate", "map", "--all", "--root-dir", source, "--exec-path", execPath); err != nil {
		t.Fatalf("transfer migrate map: %v", err)
	}
	for _, dir := range []string{source, recv} {
		if _, err := transfer.LoadSlicePin(dir); err != nil {
			t.Fatalf("load slice pin in %s: %v", dir, err)
		}
	}
	if err := run(t, "transfer", "migrate", "prove", "--all", "--root-dir", source, "--exec-path", execPath); err != nil {
		t.Fatalf("transfer migrate prove: %v", err)
	}
	rec, err := transfer.LoadReceipt(source, transfer.ProveReceiptFile)
	if err != nil || !rec.OK {
		t.Fatalf("prove receipt not ok: %+v err=%v", rec, err)
	}

	// migrate run (the state move)
	if err := run(t, "transfer", "migrate", "run", "--all", "--root-dir", source, "--exec-path", execPath); err != nil {
		t.Fatalf("transfer migrate run: %v", err)
	}
	sourceRes := stateResources(t, source)
	recvRes := stateResources(t, recv)
	if len(sourceRes) != 1 || sourceRes[0] != "random_pet.keep" {
		t.Fatalf("source state after run: %v", sourceRes)
	}
	if len(recvRes) != 3 {
		t.Fatalf("receiver state after run should hold 3 resources: %v", recvRes)
	}

	// migrate verify
	if err := run(t, "transfer", "migrate", "verify", "--all", "--root-dir", source, "--exec-path", execPath); err != nil {
		t.Fatalf("transfer migrate verify: %v", err)
	}
	vrec, err := transfer.LoadReceipt(source, transfer.VerifyReceiptFile)
	if err != nil || !vrec.OK {
		t.Fatalf("verify receipt not ok: %+v err=%v", vrec, err)
	}

	// migrate run again: pure no-op (idempotent retry)
	if err := run(t, "transfer", "migrate", "run", "--all", "--root-dir", source, "--exec-path", execPath); err != nil {
		t.Fatalf("second transfer migrate run: %v", err)
	}
	rrec, err := transfer.LoadReceipt(source, transfer.RunReceiptFile)
	if err != nil {
		t.Fatal(err)
	}
	for root, outcome := range rrec.Roots {
		if !strings.Contains(outcome, "skipped") {
			t.Fatalf("second run should skip everything, got %s=%s", root, outcome)
		}
	}
}

// TestTransfer_WiredEndToEnd drives a transfer whose moved block is still
// consumed by the source: the code move wires both sides (source rewritten in
// place to var.<input>, receiver exposing an output), the Snap CD sibling root
// gains the snapcd_module_input_from_output wiring, and the proofs thread the
// producer's value so source and receiver both plan clean.
func TestTransfer_WiredEndToEnd(t *testing.T) {
	execPath := testsupport.RequireEngine(t)
	base := testsupport.OutDir(t, "transfer-wired", "e2e")
	source := testsupport.CopyInto(t, filepath.Join(base, "source"), filepath.Join(testsupport.InDir("transfer-wired"), "source"))
	recv := testsupport.CopyInto(t, filepath.Join(base, "shared"), filepath.Join(testsupport.InDir("transfer-wired"), "shared"))
	snap := testsupport.CopyInto(t, filepath.Join(base, "snapcd"), filepath.Join(testsupport.InDir("transfer-wired"), "snapcd"))
	copyState(t, filepath.Join(testsupport.InDir("transfer-wired"), "source"), source)
	copyState(t, filepath.Join(testsupport.InDir("transfer-wired"), "shared"), recv)

	if err := run(t, "transfer", "refactor", "-y", "--root-dir", source); err != nil {
		t.Fatalf("transfer refactor: %v", err)
	}
	m, err := transfer.LoadMap(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.CrossEdges) != 1 {
		t.Fatalf("map should record 1 cross edge: %+v", m.CrossEdges)
	}
	edge := m.CrossEdges[0]
	if m.Snapcd == nil || m.Snapcd.Modules["source"] != "source_root" || m.Snapcd.Modules["../shared"] != "shared_root" {
		t.Fatalf("snapcd section: %+v", m.Snapcd)
	}

	// Source rewritten in place: keep consumes the input variable now.
	srcMain, err := os.ReadFile(filepath.Join(source, "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(srcMain), "var."+edge.Input) || strings.Contains(string(srcMain), "random_pet.move_me.id") {
		t.Fatalf("source not rewritten to var.%s:\n%s", edge.Input, srcMain)
	}
	srcVars, err := os.ReadFile(filepath.Join(source, "variables.tf"))
	if err != nil || !strings.Contains(string(srcVars), `variable "`+edge.Input+`"`) {
		t.Fatalf("source variables.tf: %v\n%s", err, srcVars)
	}
	recvOuts, err := os.ReadFile(filepath.Join(recv, "outputs.tf"))
	if err != nil || !strings.Contains(string(recvOuts), `output "`+edge.Output+`"`) {
		t.Fatalf("receiver outputs.tf: %v\n%s", err, recvOuts)
	}
	snapMain, err := os.ReadFile(filepath.Join(snap, transfer.SnapcdTargetFile))
	if err != nil || !strings.Contains(string(snapMain), `resource "snapcd_module_input_from_output" "source_root_`+edge.Input+`"`) || !strings.Contains(string(snapMain), "snapcd_module.shared_root.id") || !strings.Contains(string(snapMain), `resource "snapcd_module" "source_root"`) {
		t.Fatalf("snapcd main.tf: %v\n%s", err, snapMain)
	}

	// The migrate half proves with the producer value threaded, then moves.
	if err := run(t, "transfer", "migrate", "--all", "-y", "--root-dir", source, "--exec-path", execPath); err != nil {
		t.Fatalf("transfer migrate: %v", err)
	}
	if got := stateResources(t, source); len(got) != 1 || got[0] != "random_pet.keep" {
		t.Fatalf("source state after run: %v", got)
	}
	if got := stateResources(t, recv); len(got) != 2 {
		t.Fatalf("receiver state after run: %v", got)
	}
}

// TestTransfer_BarePipelines: the bare family commands run their steps in
// order; without a TTY the approval pause refuses unless -y approves it.
func TestTransfer_BarePipelines(t *testing.T) {
	execPath := testsupport.RequireEngine(t)
	base := testsupport.OutDir(t, "transfer", "bare")
	source := testsupport.CopyInto(t, filepath.Join(base, "source"), filepath.Join(testsupport.InDir("transfer"), "source"))
	recv := testsupport.CopyInto(t, filepath.Join(base, "shared"), filepath.Join(testsupport.InDir("transfer"), "shared"))
	copyState(t, filepath.Join(testsupport.InDir("transfer"), "source"), source)
	copyState(t, filepath.Join(testsupport.InDir("transfer"), "shared"), recv)

	// Without a TTY and without -y, the pause refuses and nothing is written.
	if err := run(t, "transfer", "refactor", "--root-dir", source); err == nil || !strings.Contains(err.Error(), "-y") {
		t.Fatalf("bare refactor without -y must refuse at the pause, got: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(recv, "main.tf")); err != nil || strings.Contains(string(b), "move_me") {
		t.Fatal("the pause refusal must not have moved code")
	}

	if err := run(t, "transfer", "refactor", "-y", "--root-dir", source); err != nil {
		t.Fatalf("bare transfer refactor: %v", err)
	}
	if err := run(t, "transfer", "migrate", "--all", "-y", "--root-dir", source, "--exec-path", execPath); err != nil {
		t.Fatalf("bare transfer migrate --all: %v", err)
	}
	if got := stateResources(t, recv); len(got) != 3 {
		t.Fatalf("receiver state after bare pipelines: %v", got)
	}
}

// TestTransfer_SliceEndToEnd plays the external orchestrator by hand on the
// wired fixture: every migrate step runs against one root with only that
// root's checkout, and the fragment, outputs, and run-receipt artifacts are
// copied between the slices' workdirs. Also exercises the two refusals that
// keep a distributed run ordered: a consumer proving before its producer's
// outputs arrive, and the source writing before the receivers' run receipts.
func TestTransfer_SliceEndToEnd(t *testing.T) {
	execPath := testsupport.RequireEngine(t)
	base := testsupport.OutDir(t, "transfer-wired", "slices")
	source := testsupport.CopyInto(t, filepath.Join(base, "source"), filepath.Join(testsupport.InDir("transfer-wired"), "source"))
	recv := testsupport.CopyInto(t, filepath.Join(base, "shared"), filepath.Join(testsupport.InDir("transfer-wired"), "shared"))
	snap := testsupport.CopyInto(t, filepath.Join(base, "snapcd"), filepath.Join(testsupport.InDir("transfer-wired"), "snapcd"))
	copyState(t, filepath.Join(testsupport.InDir("transfer-wired"), "source"), source)
	copyState(t, filepath.Join(testsupport.InDir("transfer-wired"), "shared"), recv)

	if err := run(t, "transfer", "refactor", "-y", "--root-dir", source); err != nil {
		t.Fatalf("transfer refactor: %v", err)
	}

	// The distributed map copies make every root a self-contained slice.
	for _, dir := range []string{source, recv, snap} {
		if _, err := os.Stat(filepath.Join(dir, transfer.MapFile)); err != nil {
			t.Fatalf("no map copy in %s: %v", dir, err)
		}
		if err := run(t, "transfer", "refactor", "diff", "--root-dir", dir); err != nil {
			t.Fatalf("slice diff in %s: %v", dir, err)
		}
	}

	copyArtifact := func(src, dst string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("artifact %s: %v", src, err)
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	srcWork := filepath.Join(source, transfer.WorkDirName)
	recvWork := filepath.Join(recv, transfer.WorkDirName)

	// map: source first (writes the fragment), then the receiver with it.
	if err := run(t, "transfer", "migrate", "map", "--root-dir", source, "--exec-path", execPath); err != nil {
		t.Fatalf("migrate map (source slice): %v", err)
	}
	if err := run(t, "transfer", "migrate", "map", "--root-dir", recv, "--exec-path", execPath); err == nil || !strings.Contains(err.Error(), "fragment") {
		t.Fatalf("receiver map without the fragment must refuse, got: %v", err)
	}
	copyArtifact(transfer.FragmentStateFile(srcWork, "shared"), transfer.FragmentStateFile(recvWork, "shared"))
	copyArtifact(transfer.FragmentMetaFile(srcWork, "shared"), transfer.FragmentMetaFile(recvWork, "shared"))
	if err := run(t, "transfer", "migrate", "map", "--root-dir", recv, "--exec-path", execPath); err != nil {
		t.Fatalf("migrate map (receiver slice): %v", err)
	}

	// prove: the source consumes the moved output, so the receiver goes first.
	if err := run(t, "transfer", "migrate", "prove", "--root-dir", source, "--exec-path", execPath); err == nil || !strings.Contains(err.Error(), "outputs-shared.yaml") {
		t.Fatalf("consumer prove without the producer outputs must refuse, got: %v", err)
	}
	if err := run(t, "transfer", "migrate", "prove", "--root-dir", recv, "--exec-path", execPath); err != nil {
		t.Fatalf("migrate prove (receiver slice): %v", err)
	}
	copyArtifact(transfer.OutputsFile(recvWork, "shared"), transfer.OutputsFile(srcWork, "shared"))
	if err := run(t, "transfer", "migrate", "prove", "--root-dir", source, "--exec-path", execPath); err != nil {
		t.Fatalf("migrate prove (source slice): %v", err)
	}

	// run: the source is written last and demands the receiver's run receipt.
	if err := run(t, "transfer", "migrate", "run", "--root-dir", source, "--exec-path", execPath); err == nil || !strings.Contains(err.Error(), "run receipt") {
		t.Fatalf("source run without receiver receipts must refuse, got: %v", err)
	}
	if err := run(t, "transfer", "migrate", "run", "--root-dir", recv, "--exec-path", execPath); err != nil {
		t.Fatalf("migrate run (receiver slice): %v", err)
	}
	copyArtifact(filepath.Join(recv, transfer.RunReceiptFile), transfer.RecvRunReceiptFile(srcWork, "shared"))
	if err := run(t, "transfer", "migrate", "run", "--root-dir", source, "--exec-path", execPath); err != nil {
		t.Fatalf("migrate run (source slice): %v", err)
	}
	if got := stateResources(t, source); len(got) != 1 || got[0] != "random_pet.keep" {
		t.Fatalf("source state after slice runs: %v", got)
	}
	if got := stateResources(t, recv); len(got) != 2 {
		t.Fatalf("receiver state after slice runs: %v", got)
	}

	// verify: producer first again, live values threaded onward.
	if err := run(t, "transfer", "migrate", "verify", "--root-dir", recv, "--exec-path", execPath); err != nil {
		t.Fatalf("migrate verify (receiver slice): %v", err)
	}
	copyArtifact(transfer.OutputsFile(recvWork, "shared"), transfer.OutputsFile(srcWork, "shared"))
	if err := run(t, "transfer", "migrate", "verify", "--root-dir", source, "--exec-path", execPath); err != nil {
		t.Fatalf("migrate verify (source slice): %v", err)
	}

	// A re-run of a written slice converges to a skip, not a second write.
	if err := run(t, "transfer", "migrate", "run", "--root-dir", recv, "--exec-path", execPath); err != nil {
		t.Fatalf("receiver run retry: %v", err)
	}
	rrec, err := transfer.LoadReceipt(recv, transfer.RunReceiptFile)
	if err != nil || !strings.Contains(rrec.Roots["shared"], "skipped") {
		t.Fatalf("receiver run retry should skip, got %+v err=%v", rrec, err)
	}
}
