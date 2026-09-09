// Package transfer moves selected blocks between pre-existing roots: the source
// root is decorated (`@demono:move <receiver>`), receivers are living roots
// bound by label to local working trees, and both sides' states are written.
//
// The first iteration supports self-contained selections only: any value or
// ordering edge across the transfer boundary is refused at map time, so the
// code move is purely subtractive on the source and purely additive on each
// receiver. Structural needs (variables, locals) travel with the blocks;
// wiring across the boundary is future work.
package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/hashicorp/terraform-exec/tfexec"
	"gopkg.in/yaml.v3"

	"github.com/schrieksoft/demonolith/internal/boundary"
	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/hclgraph"
	"github.com/schrieksoft/demonolith/internal/pipeline"
)

// Sidecar filenames at the source root. The map is the code half's plan; the
// migrate half writes its own map receipt (the pins) plus prove/run/verify.
const (
	MapFile           = "demonolith-transfer-map.yaml"
	MigrateMapFile    = "demonolith-transfer-migrate-map.yaml"
	ProveReceiptFile  = "demonolith-transfer-prove.yaml"
	RunReceiptFile    = "demonolith-transfer-run.yaml"
	VerifyReceiptFile = "demonolith-transfer-verify.yaml"
)

// WorkDirName holds the pulled state copies and backups under the source root.
const WorkDirName = ".demono-transfer"



// Pin identifies one root's state at map time. Lineage is the durable
// identity; serial pins the exact generation the plan was computed against.
type Pin struct {
	Lineage string `yaml:"lineage"`
	Serial  int    `yaml:"serial"`
}

// Receiver is one destination root in the map, keyed by its decorator target:
// the directory relative to the source root (e.g. "../network").
type Receiver struct {
	// Blocks are all addresses moved into this receiver (data included).
	Blocks []string `yaml:"blocks"`
	// Moves are the state-carrying addresses among them (resources, modules).
	Moves []string `yaml:"moves"`
	// Variables and Locals are the structural declarations that travel along.
	Variables []string `yaml:"variables,omitempty"`
	Locals    []string `yaml:"locals,omitempty"`
	// Inputs and Outputs are the boundary wiring this receiver declares.
	Inputs  []string `yaml:"inputs,omitempty"`
	Outputs []string `yaml:"outputs,omitempty"`
	// FileChecksums holds the sha256 of every file `transfer refactor run`
	// wrote or appended to in this receiver, keyed by filename; the diff gate
	// compares against them.
	FileChecksums map[string]string `yaml:"file_checksums,omitempty"`
}

// CrossEdge is one value wiring across the transfer boundary: consumer input
// fed from producer output. Module names are receiver targets, or the map's
// remainder for the source root.
type CrossEdge struct {
	Consumer string `yaml:"consumer"`
	Input    string `yaml:"input"`
	Producer string `yaml:"producer"`
	Output   string `yaml:"output"`
}

// OrderingEdge is one whole-module dependency with no value.
type OrderingEdge struct {
	Consumer string `yaml:"consumer"`
	Producer string `yaml:"producer"`
}

// Snapcd records the Snap CD root a transfer updates: where it is (relative to
// the source root's parent), which snapcd_module resource represents each
// involved root, and — after refactor run — the wiring file's checksum.
type Snapcd struct {
	Dir string `yaml:"dir"`
	// Modules maps a root ("source" or a receiver target) to the name of its
	// `resource "snapcd_module"` block in the Snap CD root.
	Modules map[string]string `yaml:"modules"`
	// FileChecksums holds the sha256 of every Snap CD root file the code move
	// appended to, keyed by filename.
	FileChecksums map[string]string `yaml:"file_checksums,omitempty"`
}

// Map is the reviewable transfer plan, written at the source root by the code
// half. `transfer refactor run` finalizes it with the receiver file checksums
// and distributes a byte-identical copy into every touched root; the file's
// sha256 is the transfer's identity across slices.
type Map struct {
	Version   int    `yaml:"version"`
	Created   string `yaml:"created"`
	Tool      string `yaml:"tool"`
	Remainder string `yaml:"remainder"`
	// SourceDir is the source root's directory basename — how a distributed
	// copy tells a slice which role its directory holds.
	SourceDir string              `yaml:"source_dir,omitempty"`
	Receivers map[string]Receiver `yaml:"receivers"`
	// CrossEdges and OrderingEdges are the wiring the transfer creates across
	// the boundary; the migrate half threads values along them.
	CrossEdges    []CrossEdge    `yaml:"cross_edges,omitempty"`
	OrderingEdges []OrderingEdge `yaml:"ordering_edges,omitempty"`
	// SourceFileChecksums holds the sha256 of every source file the code move
	// wrote, appended to, or rewrote, keyed by filename.
	SourceFileChecksums map[string]string `yaml:"source_file_checksums,omitempty"`
	Snapcd              *Snapcd           `yaml:"snapcd,omitempty"`
}

// IsRun reports whether the code half has executed (the map is finalized).
func (m *Map) IsRun() bool {
	for _, r := range m.Receivers {
		if len(r.FileChecksums) == 0 {
			return false
		}
	}
	return len(m.Receivers) > 0
}

// Receipt records one migrate-half step of one slice, tied to the transfer by
// map hash and to the slice's state generation by pin.
type Receipt struct {
	Version int    `yaml:"version"`
	Created string `yaml:"created"`
	Tool    string `yaml:"tool"`
	Step    string `yaml:"step"`
	MapHash string `yaml:"map_hash,omitempty"`
	Role    string `yaml:"role,omitempty"`
	Pin     Pin    `yaml:"pin"`
	OK      bool   `yaml:"ok"`
	// Roots records the per-root outcome ("zero changes", "pushed", ...).
	Roots map[string]string `yaml:"roots"`
}

// FileSHA256 hashes a file for the map's receiver-file checksum.
func FileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

// WriteMap / LoadMap round-trip the map at the source root.
func WriteMap(m *Map, rootDir string) error {
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(rootDir, MapFile), b, 0o644)
}

func LoadMap(rootDir string) (*Map, error) {
	b, err := os.ReadFile(filepath.Join(rootDir, MapFile))
	if err != nil {
		return nil, fmt.Errorf("no %s found in %s; run `demonolith transfer map` first", MapFile, rootDir)
	}
	var m Map
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func WriteReceipt(r *Receipt, rootDir, file string) error {
	b, err := yaml.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(rootDir, file), b, 0o644)
}

func LoadReceipt(rootDir, file string) (*Receipt, error) {
	b, err := os.ReadFile(filepath.Join(rootDir, file))
	if err != nil {
		return nil, err
	}
	var r Receipt
	if err := yaml.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ReceiverNames returns the map's receiver labels, sorted.
func (m *Map) ReceiverNames() []string {
	out := make([]string, 0, len(m.Receivers))
	for n := range m.Receivers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Analysis validation ------------------------------------------------------

// Plan derives the transfer's content from the source analysis, refusing
// anything the first iteration cannot carry.
type Plan struct {
	Analysis  *pipeline.Analysis
	Remainder string
	// Receivers keyed by label: block/move addresses and structural carve.
	Receivers map[string]*PlanReceiver
}

type PlanReceiver struct {
	Blocks     []string
	Moves      []string
	Structural *emit.Structural
	// Inputs and Outputs are the boundary wiring names this receiver declares.
	Inputs  []string
	Outputs []string
}

// BuildPlan analyzes the source and validates the selection is self-contained.
func BuildPlan(rootDir string) (*Plan, error) {
	a, err := pipeline.Analyze(rootDir, pipeline.Options{})
	if err != nil {
		return nil, err
	}
	p := a.Placement

	if len(p.Duplicated) > 0 {
		var addrs []string
		for addr := range p.Duplicated {
			addrs = append(addrs, addr)
		}
		sort.Strings(addrs)
		return nil, fmt.Errorf("data sources are consumed on both sides of the transfer: %s\nGive each a single side (move its consumers together), or duplicate it by hand first", strings.Join(addrs, ", "))
	}

	plan := &Plan{Analysis: a, Remainder: p.Remainder, Receivers: map[string]*PlanReceiver{}}
	for _, module := range p.ModuleNames() {
		if module == p.Remainder {
			continue
		}
		if _, err := ResolveReceiverPath(rootDir, module); err != nil {
			return nil, err
		}
		pr := &PlanReceiver{}
		for _, addr := range p.Modules[module] {
			pr.Blocks = append(pr.Blocks, addr.String())
			if addr.Kind == hclgraph.KindResource || addr.Kind == hclgraph.KindModule {
				pr.Moves = append(pr.Moves, addr.String())
			}
		}
		sort.Strings(pr.Blocks)
		sort.Strings(pr.Moves)
		st, err := emit.StructuralHCL(rootDir, a.Graph, p, a.Boundary, module)
		if err != nil {
			return nil, err
		}
		pr.Structural = st
		if b := a.Boundary.Boundaries[module]; b != nil {
			for name, in := range b.Inputs {
				if !in.External {
					pr.Inputs = append(pr.Inputs, name)
				}
			}
			for name := range b.Outputs {
				pr.Outputs = append(pr.Outputs, name)
			}
			sort.Strings(pr.Inputs)
			sort.Strings(pr.Outputs)
		}
		plan.Receivers[module] = pr
	}
	if len(plan.Receivers) == 0 {
		return nil, fmt.Errorf("no blocks are decorated for transfer; add `# @demono:move <receiver>` above the blocks to move")
	}
	// Directory basenames are the slice identity in distributed map copies and
	// artifact names; a collision would make a root's role ambiguous.
	bases := map[string]string{filepath.Base(filepath.Clean(rootDir)): "the source root"}
	names := make([]string, 0, len(plan.Receivers))
	for n := range plan.Receivers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		base := filepath.Base(name)
		if prev, ok := bases[base]; ok {
			return nil, fmt.Errorf("receiver %q shares the directory name %q with %s; slice roots are told apart by directory basename, so rename one directory first", name, base, prev)
		}
		bases[base] = fmt.Sprintf("receiver %q", name)
	}
	return plan, nil
}

// SourceWiring returns the wiring names the source root itself must declare:
// its new cross inputs (it now consumes what it gave away) and outputs (a
// receiver consumes what stays).
func (plan *Plan) SourceWiring() (inputs, outputs []string) {
	if b := plan.Analysis.Boundary.Boundaries[plan.Remainder]; b != nil {
		for name, in := range b.Inputs {
			if !in.External {
				inputs = append(inputs, name)
			}
		}
		for name := range b.Outputs {
			outputs = append(outputs, name)
		}
	}
	sort.Strings(inputs)
	sort.Strings(outputs)
	return inputs, outputs
}

// Edges converts the analysis's boundary edges into the map's form, with the
// remainder standing in for the source root.
func (plan *Plan) Edges() (cross []CrossEdge, ordering []OrderingEdge) {
	seen := map[string]bool{}
	for _, e := range plan.Analysis.Boundary.CrossEdges {
		key := e.ConsumerModule + "\x00" + e.InputName
		if seen[key] {
			continue
		}
		seen[key] = true
		cross = append(cross, CrossEdge{Consumer: e.ConsumerModule, Input: e.InputName, Producer: e.ProducerModule, Output: e.OutputName})
	}
	sort.Slice(cross, func(i, j int) bool {
		return cross[i].Consumer+cross[i].Input < cross[j].Consumer+cross[j].Input
	})
	seenO := map[string]bool{}
	for _, e := range plan.Analysis.Boundary.OrderingEdges {
		key := e.ConsumerModule + "\x00" + e.ProducerModule
		if seenO[key] {
			continue
		}
		seenO[key] = true
		ordering = append(ordering, OrderingEdge{Consumer: e.ConsumerModule, Producer: e.ProducerModule})
	}
	sort.Slice(ordering, func(i, j int) bool {
		return ordering[i].Consumer+ordering[i].Producer < ordering[j].Consumer+ordering[j].Producer
	})
	return cross, ordering
}

// BoundaryFromMap reconstructs the boundary the migrate half threads over —
// after the code move the source carries no decorators, so the map is the
// record. Module names are receiver targets plus the remainder.
func BoundaryFromMap(m *Map) *boundary.Result {
	res := &boundary.Result{Boundaries: map[string]*boundary.ModuleBoundary{}}
	get := func(name string) *boundary.ModuleBoundary {
		if res.Boundaries[name] == nil {
			res.Boundaries[name] = &boundary.ModuleBoundary{Module: name, Inputs: map[string]boundary.Input{}, Outputs: map[string]boundary.Output{}}
		}
		return res.Boundaries[name]
	}
	get(m.Remainder)
	for _, name := range m.ReceiverNames() {
		get(name)
	}
	for _, e := range m.CrossEdges {
		get(e.Consumer).Inputs[e.Input] = boundary.Input{Name: e.Input, FromModule: e.Producer, FromOutput: e.Output}
		get(e.Producer).Outputs[e.Output] = boundary.Output{Name: e.Output}
		res.CrossEdges = append(res.CrossEdges, boundary.CrossEdge{ConsumerModule: e.Consumer, InputName: e.Input, ProducerModule: e.Producer, OutputName: e.Output})
	}
	for _, e := range m.OrderingEdges {
		res.OrderingEdges = append(res.OrderingEdges, boundary.OrderingEdge{ConsumerModule: e.Consumer, ProducerModule: e.Producer})
	}
	return res
}

// ResolveReceiverPath resolves a decorator target against the source root. A
// transfer target is a relative directory (./x or ../x) pointing at an
// existing root — never a bare name (that is the split's decorator shape) and
// never an absolute path (clone-specific).
func ResolveReceiverPath(rootDir, target string) (string, error) {
	if filepath.IsAbs(target) {
		return "", fmt.Errorf("receiver %q is an absolute path; write the decorator as a directory relative to the source root, e.g. `# @demono:move ../%s`", target, filepath.Base(target))
	}
	if !strings.HasPrefix(target, "./") && !strings.HasPrefix(target, "../") {
		return "", fmt.Errorf("receiver %q is not a relative directory; a transfer moves blocks into an existing root, written as a path relative to the source root, e.g. `# @demono:move ../%s`", target, target)
	}
	abs := filepath.Clean(filepath.Join(rootDir, target))
	if abs == filepath.Clean(rootDir) {
		return "", fmt.Errorf("receiver %q resolves to the source root itself", target)
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("receiver %q: no directory at %s — a transfer target must already exist", target, abs)
	}
	return abs, nil
}

// ResolveReceivers resolves every map receiver against the source root.
func ResolveReceivers(m *Map, rootDir string) (map[string]string, error) {
	out := map[string]string{}
	for _, name := range m.ReceiverNames() {
		abs, err := ResolveReceiverPath(rootDir, name)
		if err != nil {
			return nil, err
		}
		out[name] = abs
	}
	return out, nil
}

// Slug turns a receiver target into a filename-safe token for workdir files.
func Slug(target string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, target)
	sum := sha256.Sum256([]byte(target))
	return fmt.Sprintf("%s-%x", strings.Trim(safe, "_"), sum[:4])
}

// MatchesMap reports whether the plan's selection equals the map's — the
// staleness gate every later step runs before touching anything.
func (plan *Plan) MatchesMap(m *Map) error {
	if len(plan.Receivers) != len(m.Receivers) {
		return fmt.Errorf("the source's decorated selection no longer matches %s; re-run `demonolith transfer map`", MapFile)
	}
	for name, pr := range plan.Receivers {
		mr, ok := m.Receivers[name]
		if !ok || !equalStrings(pr.Blocks, mr.Blocks) || !equalStrings(pr.Moves, mr.Moves) {
			return fmt.Errorf("the source's decorated selection for %q no longer matches %s; re-run `demonolith transfer map`", name, MapFile)
		}
	}
	cross, ordering := plan.Edges()
	if len(cross) != len(m.CrossEdges) || len(ordering) != len(m.OrderingEdges) {
		return fmt.Errorf("the wiring across the boundary no longer matches %s; re-run `demonolith transfer map`", MapFile)
	}
	for i := range cross {
		if cross[i] != m.CrossEdges[i] {
			return fmt.Errorf("the wiring across the boundary no longer matches %s; re-run `demonolith transfer map`", MapFile)
		}
	}
	for i := range ordering {
		if ordering[i] != m.OrderingEdges[i] {
			return fmt.Errorf("the wiring across the boundary no longer matches %s; re-run `demonolith transfer map`", MapFile)
		}
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Receiver-side validation -------------------------------------------------

// ValidateReceiverDir refuses a receiver working tree the code move cannot
// land in: a declaration collision
// with what must travel along — carried declarations and boundary wiring.
func ValidateReceiverDir(dir string, st *emit.Structural, inputs, outputs []string) error {
	vars, locals, outs, err := declaredNames(dir)
	if err != nil {
		return err
	}
	var clash []string
	for _, v := range st.VarNames {
		if vars[v] {
			clash = append(clash, "variable "+v)
		}
	}
	for _, v := range inputs {
		if vars[v] {
			clash = append(clash, "variable "+v)
		}
	}
	for _, l := range st.LocalNames {
		if locals[l] {
			clash = append(clash, "local "+l)
		}
	}
	for _, o := range outputs {
		if outs[o] {
			clash = append(clash, "output "+o)
		}
	}
	if len(clash) > 0 {
		sort.Strings(clash)
		return fmt.Errorf("receiver %s already declares: %s — the moved blocks carry declarations of the same names, and merging them is a human decision; rename on one side first", dir, strings.Join(clash, ", "))
	}
	return nil
}

// ValidateSourceWiring refuses when the source root already declares a name
// its new wiring needs.
func ValidateSourceWiring(rootDir string, inputs, outputs []string) error {
	vars, _, outs, err := declaredNames(rootDir)
	if err != nil {
		return err
	}
	var clash []string
	for _, v := range inputs {
		if vars[v] {
			clash = append(clash, "variable "+v)
		}
	}
	for _, o := range outputs {
		if outs[o] {
			clash = append(clash, "output "+o)
		}
	}
	if len(clash) > 0 {
		sort.Strings(clash)
		return fmt.Errorf("the source root already declares: %s — the transfer's wiring needs those names; rename the existing declarations first", strings.Join(clash, ", "))
	}
	return nil
}

// declaredNames collects the variable, local, and output names a root declares.
func declaredNames(dir string) (vars, locals, outs map[string]bool, err error) {
	vars, locals, outs = map[string]bool{}, map[string]bool{}, map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tf") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, nil, nil, err
		}
		f, diags := hclwrite.ParseConfig(src, e.Name(), hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return nil, nil, nil, fmt.Errorf("parse %s: %s", filepath.Join(dir, e.Name()), diags.Error())
		}
		for _, blk := range f.Body().Blocks() {
			switch blk.Type() {
			case "variable":
				if len(blk.Labels()) == 1 {
					vars[blk.Labels()[0]] = true
				}
			case "locals":
				for name := range blk.Body().Attributes() {
					locals[name] = true
				}
			case "output":
				if len(blk.Labels()) == 1 {
					outs[blk.Labels()[0]] = true
				}
			}
		}
	}
	return vars, locals, outs, nil
}

// Code move ----------------------------------------------------------------

// ReceiverFiles builds what the code move appends into one receiver, keyed by
// the conventional filename: variable declarations (carried and wiring) into
// variables.tf, locals and the moved blocks (cross refs rewritten to
// var.<input>) into main.tf, outputs into outputs.tf.
func ReceiverFiles(rootDir string, plan *Plan, name string) (map[string]string, error) {
	a := plan.Analysis
	moved, err := emit.MovedBlocksWiredHCL(rootDir, a.Graph, a.Placement, a.Boundary, name)
	if err != nil {
		return nil, err
	}
	varsHCL, outsHCL := emit.WiringHCL(a.Boundary, name)
	st := plan.Receivers[name].Structural
	out := map[string]string{}
	if st.VarHCL != "" || varsHCL != "" {
		out["variables.tf"] = st.VarHCL + varsHCL
	}
	var mainB strings.Builder
	if st.LocalsHCL != "" {
		mainB.WriteString(st.LocalsHCL)
		mainB.WriteString("\n")
	}
	mainB.WriteString(moved)
	out["main.tf"] = mainB.String()
	if outsHCL != "" {
		out["outputs.tf"] = outsHCL
	}
	return out, nil
}

// SourceFiles builds what the code move appends into the source root itself:
// the variables for what it now consumes from the receivers (variables.tf) and
// the outputs for what the receivers consume from it (outputs.tf). Empty when
// the source needs neither.
func SourceFiles(plan *Plan) map[string]string {
	varsHCL, outsHCL := emit.WiringHCL(plan.Analysis.Boundary, plan.Remainder)
	out := map[string]string{}
	if varsHCL != "" {
		out["variables.tf"] = varsHCL
	}
	if outsHCL != "" {
		out["outputs.tf"] = outsHCL
	}
	return out
}

// AppendToFile appends content into dir/name, creating the file when absent
// and separating from existing content with a blank line.
func AppendToFile(dir, name, content string) error {
	path := filepath.Join(dir, name)
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var b strings.Builder
	if len(existing) > 0 {
		b.Write(existing)
		if !strings.HasSuffix(string(existing), "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(strings.TrimLeft(content, "\n"))
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// RewriteSourceRefs rewrites the source root's remaining code in place so it
// consumes its former blocks as var.<input>, returning the changed files.
func RewriteSourceRefs(rootDir string, plan *Plan) ([]string, error) {
	a := plan.Analysis
	return emit.RewriteRefsInPlace(rootDir, a.Graph, a.Placement, a.Boundary, plan.Remainder)
}

var moveDecoratorRe = regexp.MustCompile(`^\s*(#|//)\s*@demono:move\s+(\S+)\s*$`)

// RemoveFromSource deletes the moved blocks (and their move decorators for the
// given receivers) from the source's files, in place.
func RemoveFromSource(rootDir string, moved map[string]bool, receivers map[string]bool) error {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tf") {
			continue
		}
		path := filepath.Join(rootDir, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, diags := hclwrite.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return fmt.Errorf("parse %s: %s", path, diags.Error())
		}
		changed := false
		for _, blk := range f.Body().Blocks() {
			if addr, ok := emit.AddrOfBlock(blk); ok && moved[addr] {
				f.Body().RemoveBlock(blk)
				changed = true
			}
		}
		if !changed {
			continue
		}
		// The block removal can orphan its decorator comment; drop those lines.
		var kept []string
		for _, line := range strings.Split(string(hclwrite.Format(f.Bytes())), "\n") {
			if mm := moveDecoratorRe.FindStringSubmatch(line); mm != nil && receivers[mm[2]] {
				continue
			}
			kept = append(kept, line)
		}
		if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// CodeMoved verifies the code act happened: no moved address parses in the
// source, and every one parses in its receiver.
func CodeMoved(rootDir string, m *Map, paths map[string]string) error {
	sourceAddrs, err := DirAddrs(rootDir)
	if err != nil {
		return err
	}
	var problems []string
	for _, name := range m.ReceiverNames() {
		recvAddrs, err := DirAddrs(paths[name])
		if err != nil {
			return err
		}
		for _, addr := range m.Receivers[name].Blocks {
			if sourceAddrs[addr] {
				problems = append(problems, fmt.Sprintf("%s still present in the source", addr))
			}
			if !recvAddrs[addr] {
				problems = append(problems, fmt.Sprintf("%s missing from receiver %q", addr, name))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("the code move has not happened (run `demonolith transfer code`):\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// DirAddrs parses a root and returns the set of block addresses it declares.
func DirAddrs(dir string) (map[string]bool, error) {
	g, err := hclgraph.ParseDir(dir)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", dir, err)
	}
	out := map[string]bool{}
	for addr := range g.Nodes {
		out[addr] = true
	}
	return out, nil
}

// State helpers ------------------------------------------------------------

// Meta is the identity header of a state file.
type Meta struct {
	Lineage string `json:"lineage"`
	Serial  int    `json:"serial"`
}

func ReadMeta(path string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("parse state %s: %w", path, err)
	}
	return m, nil
}

// BumpSerial rewrites the state file with serial set to the given value.
func BumpSerial(path string, serial int) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	raw["serial"] = serial
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// ContainsAddrs reports which of the given state-carrying addresses are
// present in the state file.
func ContainsAddrs(path string, addrs []string) (present, absent []string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var raw struct {
		Resources []struct {
			Module string `json:"module"`
			Mode   string `json:"mode"`
			Type   string `json:"type"`
			Name   string `json:"name"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	have := map[string]bool{}
	for _, r := range raw.Resources {
		if r.Mode != "managed" {
			continue
		}
		addr := r.Type + "." + r.Name
		if r.Module != "" {
			addr = r.Module + "." + addr
			// A whole-module move addresses the module itself.
			have[r.Module] = true
		}
		have[addr] = true
	}
	for _, a := range addrs {
		if have[a] {
			present = append(present, a)
		} else {
			absent = append(absent, a)
		}
	}
	return present, absent, nil
}

// PullState inits dir against its real backend and writes its state to out.
func PullState(ctx context.Context, dir, out, execPath string) error {
	tf, err := tfexec.NewTerraform(dir, execPath)
	if err != nil {
		return err
	}
	if err := tf.Init(ctx); err != nil {
		return fmt.Errorf("init %s: %w", dir, err)
	}
	state, err := tf.StatePull(ctx)
	if err != nil {
		return fmt.Errorf("state pull %s: %w", dir, err)
	}
	return os.WriteFile(out, []byte(state), 0o600)
}

// PushState pushes the local state file into dir's real backend (never forced).
func PushState(ctx context.Context, dir, path, execPath string) error {
	tf, err := tfexec.NewTerraform(dir, execPath)
	if err != nil {
		return err
	}
	if err := tf.Init(ctx); err != nil {
		return fmt.Errorf("init %s: %w", dir, err)
	}
	if err := tf.StatePush(ctx, path); err != nil {
		return fmt.Errorf("state push %s: %w", dir, err)
	}
	return nil
}

// ApplyMoves executes the state moves from sourceState into recvState, both
// local files mutated in place.
func ApplyMoves(ctx context.Context, rootDir, sourceState, recvState string, moves []string, execPath string) error {
	tf, err := tfexec.NewTerraform(rootDir, execPath)
	if err != nil {
		return err
	}
	for _, addr := range moves {
		if err := tf.StateMv(ctx, addr, addr, tfexec.State(sourceState), tfexec.StateOut(recvState)); err != nil {
			return fmt.Errorf("state mv %s: %w", addr, err)
		}
	}
	return nil
}

// EnsureWorkDir creates the gitignored state workdir under the source root.
func EnsureWorkDir(rootDir string) (string, error) {
	dir := filepath.Join(rootDir, WorkDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n"), 0o644); err != nil {
		return "", err
	}
	return dir, nil
}

func CopyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}
