package transfer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRoot writes a set of .tf files into a fresh temp dir.
func writeRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// writeSourceRoot writes a source root plus empty sibling receiver dirs, the
// layout the relative decorator targets (../shared) resolve against.
func writeSourceRoot(t *testing.T, files map[string]string, siblings ...string) string {
	t.Helper()
	base := t.TempDir()
	src := filepath.Join(base, "source")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, sib := range siblings {
		if err := os.MkdirAll(filepath.Join(base, sib), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

const sourceSrc = `
variable "size" {
  type    = number
  default = 2
}

locals {
  len = var.size
}

resource "fake_thing" "stays" {
  size = var.size
}

# @demono:move ../shared
resource "fake_thing" "goes" {
  size = local.len
}

data "fake_lookup" "goes_data" {
  id = 1
}

# @demono:move ../shared
resource "fake_other" "consumer" {
  ref = data.fake_lookup.goes_data.id
}

# @demono:move ../elsewhere
resource "fake_thing" "goes_far" {
  size = 1
}
`

func TestBuildPlan_SelfContainedSelection(t *testing.T) {
	dir := writeSourceRoot(t, map[string]string{"main.tf": sourceSrc}, "shared", "elsewhere")
	plan, err := BuildPlan(dir)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Receivers) != 2 {
		t.Fatalf("want 2 receivers, got %v", plan.Receivers)
	}
	sh := plan.Receivers["../shared"]
	if sh == nil {
		t.Fatal("receiver shared missing")
	}
	wantBlocks := []string{"data.fake_lookup.goes_data", "fake_other.consumer", "fake_thing.goes"}
	if !equalStrings(sh.Blocks, wantBlocks) {
		t.Fatalf("shared blocks: %v", sh.Blocks)
	}
	// The data source travels in code but carries no state move.
	wantMoves := []string{"fake_other.consumer", "fake_thing.goes"}
	if !equalStrings(sh.Moves, wantMoves) {
		t.Fatalf("shared moves: %v", sh.Moves)
	}
	// Structural carve: fake_thing.goes uses local.len, which uses var.size.
	if !equalStrings(sh.Structural.VarNames, []string{"size"}) || !equalStrings(sh.Structural.LocalNames, []string{"len"}) {
		t.Fatalf("structural carve: vars=%v locals=%v", sh.Structural.VarNames, sh.Structural.LocalNames)
	}
	far := plan.Receivers["../elsewhere"]
	if far == nil || !equalStrings(far.Blocks, []string{"fake_thing.goes_far"}) {
		t.Fatalf("elsewhere receiver: %+v", far)
	}
}

func TestBuildPlan_WiresCrossValueEdge(t *testing.T) {
	dir := writeSourceRoot(t, map[string]string{"main.tf": `
resource "fake_thing" "stays" {
  ref = fake_thing.goes.id
}

# @demono:move ../shared
resource "fake_thing" "goes" {
  size = 1
}
`}, "shared")
	plan, err := BuildPlan(dir)
	if err != nil {
		t.Fatalf("a cross value edge must be wired, not refused: %v", err)
	}
	cross, ordering := plan.Edges()
	if len(cross) != 1 || len(ordering) != 0 {
		t.Fatalf("edges: cross=%v ordering=%v", cross, ordering)
	}
	e := cross[0]
	if e.Consumer != plan.Remainder || e.Producer != "../shared" || e.Input == "" || e.Output == "" {
		t.Fatalf("cross edge: %+v", e)
	}
	srcIn, srcOut := plan.SourceWiring()
	if len(srcIn) != 1 || len(srcOut) != 0 {
		t.Fatalf("source wiring: in=%v out=%v", srcIn, srcOut)
	}
	if !equalStrings(plan.Receivers["../shared"].Outputs, []string{e.Output}) {
		t.Fatalf("receiver outputs: %v", plan.Receivers["../shared"].Outputs)
	}
}

func TestBuildPlan_WiresCrossOrderingEdge(t *testing.T) {
	dir := writeSourceRoot(t, map[string]string{"main.tf": `
resource "fake_thing" "stays" {
  size = 1
}

# @demono:move ../shared
resource "fake_thing" "goes" {
  size       = 1
  depends_on = [fake_thing.stays]
}
`}, "shared")
	plan, err := BuildPlan(dir)
	if err != nil {
		t.Fatalf("a cross ordering edge must be wired, not refused: %v", err)
	}
	cross, ordering := plan.Edges()
	if len(cross) != 0 || len(ordering) != 1 {
		t.Fatalf("edges: cross=%v ordering=%v", cross, ordering)
	}
	if ordering[0].Consumer != "../shared" || ordering[0].Producer != plan.Remainder {
		t.Fatalf("ordering edge: %+v", ordering[0])
	}
}

func TestBuildPlan_RefusesBothSidesDataSource(t *testing.T) {
	dir := writeSourceRoot(t, map[string]string{"main.tf": `
data "fake_lookup" "both" {
  id = 1
}

resource "fake_thing" "stays" {
  ref = data.fake_lookup.both.id
}

# @demono:move ../shared
resource "fake_thing" "goes" {
  ref = data.fake_lookup.both.id
}
`})
	_, err := BuildPlan(dir)
	if err == nil || !strings.Contains(err.Error(), "both sides") {
		t.Fatalf("want both-sides data refusal, got: %v", err)
	}
}

func TestBuildPlan_RefusesEmptySelection(t *testing.T) {
	dir := writeSourceRoot(t, map[string]string{"main.tf": `
resource "fake_thing" "stays" {
  size = 1
}
`})
	_, err := BuildPlan(dir)
	if err == nil || !strings.Contains(err.Error(), "no blocks are decorated") {
		t.Fatalf("want empty-selection refusal, got: %v", err)
	}
}

func TestMatchesMap(t *testing.T) {
	dir := writeSourceRoot(t, map[string]string{"main.tf": sourceSrc}, "shared", "elsewhere")
	plan, err := BuildPlan(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := &Map{Receivers: map[string]Receiver{}}
	for name, pr := range plan.Receivers {
		m.Receivers[name] = Receiver{Blocks: pr.Blocks, Moves: pr.Moves}
	}
	if err := plan.MatchesMap(m); err != nil {
		t.Fatalf("identical selection must match: %v", err)
	}

	changed := m.Receivers["../shared"]
	changed.Blocks = append([]string{}, changed.Blocks[1:]...)
	m.Receivers["../shared"] = changed
	if err := plan.MatchesMap(m); err == nil || !strings.Contains(err.Error(), "re-run") {
		t.Fatalf("changed blocks must mismatch, got: %v", err)
	}

	delete(m.Receivers, "../shared")
	if err := plan.MatchesMap(m); err == nil {
		t.Fatal("missing receiver must mismatch")
	}
}

func TestValidateReceiverDir(t *testing.T) {
	dir := writeSourceRoot(t, map[string]string{"main.tf": sourceSrc}, "shared", "elsewhere")
	plan, err := BuildPlan(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := plan.Receivers["../shared"].Structural

	clean := writeRoot(t, map[string]string{"main.tf": `resource "fake_thing" "mine" {}`})
	if err := ValidateReceiverDir(clean, st, nil, nil); err != nil {
		t.Fatalf("clean receiver must validate: %v", err)
	}

	varClash := writeRoot(t, map[string]string{"variables.tf": `variable "size" { type = number }`})
	if err := ValidateReceiverDir(varClash, st, nil, nil); err == nil || !strings.Contains(err.Error(), "variable size") {
		t.Fatalf("variable collision must refuse, got: %v", err)
	}

	localClash := writeRoot(t, map[string]string{"main.tf": `locals { len = 1 }`})
	if err := ValidateReceiverDir(localClash, st, nil, nil); err == nil || !strings.Contains(err.Error(), "local len") {
		t.Fatalf("local collision must refuse, got: %v", err)
	}
}

func TestReceiverFiles(t *testing.T) {
	dir := writeSourceRoot(t, map[string]string{"main.tf": sourceSrc}, "shared", "elsewhere")
	plan, err := BuildPlan(dir)
	if err != nil {
		t.Fatal(err)
	}
	files, err := ReceiverFiles(dir, plan, "../shared")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(files["variables.tf"], `variable "size"`) {
		t.Fatalf("variables.tf: %q", files["variables.tf"])
	}
	main := files["main.tf"]
	for _, want := range []string{"locals {", "len = var.size", `resource "fake_thing" "goes"`, `data "fake_lookup" "goes_data"`, `resource "fake_other" "consumer"`} {
		if !strings.Contains(main, want) {
			t.Fatalf("main.tf missing %q:\n%s", want, main)
		}
	}
	if strings.Contains(main, "@demono") {
		t.Fatalf("decorators must be stripped:\n%s", main)
	}
	if strings.Contains(main, "goes_far") || strings.Contains(main, `"stays"`) {
		t.Fatalf("main.tf must only carry its own blocks:\n%s", main)
	}
	if _, ok := files["outputs.tf"]; ok {
		t.Fatalf("no outputs expected: %q", files["outputs.tf"])
	}
}

func TestAppendToFile(t *testing.T) {
	dir := t.TempDir()
	if err := AppendToFile(dir, "variables.tf", "variable \"a\" {}\n"); err != nil {
		t.Fatal(err)
	}
	if err := AppendToFile(dir, "variables.tf", "variable \"b\" {}\n"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "variables.tf"))
	if err != nil {
		t.Fatal(err)
	}
	want := "variable \"a\" {}\n\nvariable \"b\" {}\n"
	if string(b) != want {
		t.Fatalf("appended file:\n%q\nwant:\n%q", b, want)
	}
}

func TestRemoveFromSource(t *testing.T) {
	dir := writeRoot(t, map[string]string{"main.tf": sourceSrc})
	moved := map[string]bool{"fake_thing.goes": true, "data.fake_lookup.goes_data": true, "fake_other.consumer": true}
	receivers := map[string]bool{"../shared": true}
	if err := RemoveFromSource(dir, moved, receivers); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(got)
	for _, gone := range []string{`"goes"`, "goes_data", `"consumer"`, "@demono:move ../shared"} {
		if strings.Contains(src, gone) {
			t.Fatalf("source still contains %q:\n%s", gone, src)
		}
	}
	// Blocks and decorators for other receivers stay.
	for _, kept := range []string{`resource "fake_thing" "stays"`, `variable "size"`, "locals {", "@demono:move ../elsewhere", "goes_far"} {
		if !strings.Contains(src, kept) {
			t.Fatalf("source lost %q:\n%s", kept, src)
		}
	}
}

func TestCodeMoved(t *testing.T) {
	source := writeRoot(t, map[string]string{"main.tf": sourceSrc})
	recv := writeRoot(t, map[string]string{"main.tf": `resource "fake_thing" "mine" {}`})
	m := &Map{Receivers: map[string]Receiver{
		"../shared": {Blocks: []string{"fake_thing.goes"}},
	}}
	paths := map[string]string{"../shared": recv}

	err := CodeMoved(source, m, paths)
	if err == nil || !strings.Contains(err.Error(), "still present in the source") || !strings.Contains(err.Error(), "missing from receiver") {
		t.Fatalf("unmoved code must fail both ways, got: %v", err)
	}

	if err := RemoveFromSource(source, map[string]bool{"fake_thing.goes": true}, map[string]bool{"../shared": true}); err != nil {
		t.Fatal(err)
	}
	if err := AppendToFile(recv, "main.tf", `resource "fake_thing" "goes" {}`); err != nil {
		t.Fatal(err)
	}
	if err := CodeMoved(source, m, paths); err != nil {
		t.Fatalf("moved code must pass: %v", err)
	}
}

func writeState(t *testing.T, dir string, serial int, lineage string, resources []map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, "state.tfstate")
	b, err := json.Marshal(map[string]any{
		"version": 4, "serial": serial, "lineage": lineage, "resources": resources,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStateMetaAndSerialBump(t *testing.T) {
	path := writeState(t, t.TempDir(), 7, "abc-123", nil)
	m, err := ReadMeta(path)
	if err != nil || m.Lineage != "abc-123" || m.Serial != 7 {
		t.Fatalf("meta: %+v err=%v", m, err)
	}
	if err := BumpSerial(path, 8); err != nil {
		t.Fatal(err)
	}
	m, err = ReadMeta(path)
	if err != nil || m.Serial != 8 || m.Lineage != "abc-123" {
		t.Fatalf("bumped meta: %+v err=%v", m, err)
	}
}

func TestContainsAddrs(t *testing.T) {
	path := writeState(t, t.TempDir(), 1, "l", []map[string]any{
		{"mode": "managed", "type": "fake_thing", "name": "a"},
		{"mode": "managed", "type": "fake_thing", "name": "b", "module": "module.child"},
		{"mode": "data", "type": "fake_lookup", "name": "d"},
	})
	present, absent, err := ContainsAddrs(path, []string{"fake_thing.a", "module.child", "fake_thing.missing", "data.fake_lookup.d"})
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(present, []string{"fake_thing.a", "module.child"}) {
		t.Fatalf("present: %v", present)
	}
	// Data sources carry no managed state entry and never count as present.
	if !equalStrings(absent, []string{"fake_thing.missing", "data.fake_lookup.d"}) {
		t.Fatalf("absent: %v", absent)
	}
}

func TestMapPinsReceiptRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &Map{Version: 1, Created: "2026-08-24T00:00:00Z", Tool: "test", Remainder: "legacy",
		Receivers: map[string]Receiver{"../shared": {Blocks: []string{"a.b"}, Moves: []string{"a.b"}}}}
	if m.IsRun() {
		t.Fatal("map without checksums must not be run")
	}
	if err := WriteMap(m, dir); err != nil {
		t.Fatal(err)
	}
	got, err := LoadMap(dir)
	if err != nil || len(got.Receivers["../shared"].Blocks) != 1 {
		t.Fatalf("map round-trip: %+v err=%v", got, err)
	}
	r := m.Receivers["../shared"]
	r.FileChecksums = map[string]string{"main.tf": "deadbeef"}
	m.Receivers["../shared"] = r
	if !m.IsRun() {
		t.Fatal("map with checksums must be run")
	}

	pin := &SlicePin{Version: 1, MapHash: "abc123", Role: "source", Pin: Pin{Lineage: "l", Serial: 3}}
	if err := WriteSlicePin(pin, dir); err != nil {
		t.Fatal(err)
	}
	gp, err := LoadSlicePin(dir)
	if err != nil || gp.Pin != pin.Pin || gp.MapHash != "abc123" || gp.Role != "source" {
		t.Fatalf("slice pin round-trip: %+v err=%v", gp, err)
	}

	rec := &Receipt{Version: 1, Step: "prove", MapHash: "abc123", Role: "source", Pin: pin.Pin, OK: true, Roots: map[string]string{"source": "zero changes"}}
	if err := WriteReceipt(rec, dir, ProveReceiptFile); err != nil {
		t.Fatal(err)
	}
	gr, err := LoadReceipt(dir, ProveReceiptFile)
	if err != nil || !gr.OK || gr.Pin != pin.Pin || gr.MapHash != "abc123" || gr.Roots["source"] != "zero changes" {
		t.Fatalf("receipt round-trip: %+v err=%v", gr, err)
	}
}

func TestRoleOf(t *testing.T) {
	m := &Map{Remainder: "legacy", SourceDir: "platform",
		Receivers: map[string]Receiver{"../network": {}, "../deep/storage": {}},
		Snapcd:    &Snapcd{Dir: "snapcd"}}
	cases := []struct{ dir, kind, key, base string }{
		{"/work/platform", "source", "", "platform"},
		{"/work/network", "receiver", "../network", "network"},
		{"/elsewhere/storage", "receiver", "../deep/storage", "storage"},
		{"/work/snapcd", "snapcd", "", "snapcd"},
	}
	for _, c := range cases {
		r, err := RoleOf(m, c.dir)
		if err != nil || r.Kind != c.kind || r.Key != c.key || r.Base != c.base {
			t.Fatalf("RoleOf(%s) = %+v err=%v, want kind=%s key=%s base=%s", c.dir, r, err, c.kind, c.key, c.base)
		}
	}
	if _, err := RoleOf(m, "/work/unrelated"); err == nil || !strings.Contains(err.Error(), "not part of this transfer") {
		t.Fatalf("unrelated dir must refuse, got: %v", err)
	}
	if _, err := RoleOf(&Map{Remainder: "legacy"}, "/work/platform"); err == nil || !strings.Contains(err.Error(), "does not name its source root") {
		t.Fatalf("map without source_dir must refuse, got: %v", err)
	}
	if got := (Role{Kind: "source"}).Module(m); got != "legacy" {
		t.Fatalf("source module = %q", got)
	}
	if got := BaseForModule(m, "legacy"); got != "platform" {
		t.Fatalf("BaseForModule(remainder) = %q", got)
	}
	if got := BaseForModule(m, "../network"); got != "network" {
		t.Fatalf("BaseForModule(receiver) = %q", got)
	}
}

func TestBuildPlan_RefusesBasenameCollision(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "source")
	for _, d := range []string{src, filepath.Join(base, "shared"), filepath.Join(base, "deep", "shared")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	main := `
# @demono:move ../shared
resource "fake_thing" "a" {
  size = 1
}

# @demono:move ../deep/shared
resource "fake_thing" "b" {
  size = 1
}
`
	if err := os.WriteFile(filepath.Join(src, "main.tf"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := BuildPlan(src)
	if err == nil || !strings.Contains(err.Error(), "directory name") {
		t.Fatalf("basename collision must refuse, got: %v", err)
	}
}

func TestMapHashAndDistribute(t *testing.T) {
	src := t.TempDir()
	recv := t.TempDir()
	m := &Map{Version: 1, Remainder: "legacy", SourceDir: "source", Receivers: map[string]Receiver{"../shared": {Blocks: []string{"a.b"}}}}
	if err := WriteMap(m, src); err != nil {
		t.Fatal(err)
	}
	h1, err := MapHash(src)
	if err != nil || h1 == "" {
		t.Fatalf("map hash: %q err=%v", h1, err)
	}
	if err := DistributeMap(src, []string{recv}); err != nil {
		t.Fatal(err)
	}
	h2, err := MapHash(recv)
	if err != nil || h2 != h1 {
		t.Fatalf("distributed copy hash %q != source %q (err=%v)", h2, h1, err)
	}
	if err := MapCopiesIdentical(src, map[string]string{"receiver ../shared": recv}); err != nil {
		t.Fatalf("identical copies must pass: %v", err)
	}
	if err := os.WriteFile(filepath.Join(recv, MapFile), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MapCopiesIdentical(src, map[string]string{"receiver ../shared": recv}); err == nil || !strings.Contains(err.Error(), "byte-identical") {
		t.Fatalf("tampered copy must refuse, got: %v", err)
	}
}

func TestFragmentAndOutputsRoundTrip(t *testing.T) {
	work := t.TempDir()
	fm := &FragmentMeta{Version: 1, MapHash: "abc", Receiver: "network", SourcePin: Pin{Lineage: "l", Serial: 5}, Moves: []string{"a.b"}}
	if err := WriteFragmentMeta(fm, work); err != nil {
		t.Fatal(err)
	}
	gf, err := LoadFragmentMeta(work, "network")
	if err != nil || gf.MapHash != "abc" || gf.SourcePin != fm.SourcePin || len(gf.Moves) != 1 {
		t.Fatalf("fragment meta round-trip: %+v err=%v", gf, err)
	}
	oa := &OutputsArtifact{Version: 1, MapHash: "abc", Role: "network", Outputs: map[string]string{"vpc_name": "quiet-owl"}}
	if err := WriteOutputs(oa, work); err != nil {
		t.Fatal(err)
	}
	go2, err := LoadOutputs(work, "network")
	if err != nil || go2.Outputs["vpc_name"] != "quiet-owl" || go2.MapHash != "abc" {
		t.Fatalf("outputs round-trip: %+v err=%v", go2, err)
	}
}

// TestBuildPlan_RefusesNonPathTarget: a bare-name target (the split's
// decorator shape) is refused with guidance toward the relative-path form.
func TestBuildPlan_RefusesNonPathTarget(t *testing.T) {
	dir := writeSourceRoot(t, map[string]string{"main.tf": `
# @demono:move shared
resource "fake_thing" "goes" {
  size = 1
}
`}, "shared")
	_, err := BuildPlan(dir)
	if err == nil || !strings.Contains(err.Error(), "relative directory") {
		t.Fatalf("bare-name target must refuse, got: %v", err)
	}
}
