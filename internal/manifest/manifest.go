// Package manifest defines the demonolith-refactor manifest: the durable,
// versioned contract between `refactor` (which computes what must move and how
// modules wire together) and `migrate`/`prove` (which replay that plan without
// re-deriving it). The schema is a public API: PR reviewers read it, CI parses
// it, and a control plane may ingest it, so changes within a major version must
// be additive only.
package manifest

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/pipeline"
	"github.com/schrieksoft/demonolith/internal/statemove"
)

// SchemaVersion is the current manifest major version. Consumers refuse a
// manifest whose version they do not know rather than guessing.
const SchemaVersion = 1

// FileName is the single canonical manifest per root: refactor always
// re-derives the full plan from the monolith source, so each run overwrites it
// in place and history lives in version control.
const FileName = "demonolith-refactor.yaml"

// TimeLayout is the compact UTC timestamp used in receipt and verdict
// filenames, keeping lexical order equal to date order.
const TimeLayout = "20060102-150405"

// Manifest is the full refactor plan for one monolith root.
type Manifest struct {
	Version int    `yaml:"version"`
	Created string `yaml:"created"`
	Tool    string `yaml:"tool"`
	Source  Source `yaml:"source"`
	Output  Output `yaml:"output"`
	// Modules maps a module name to its emitted root and assigned blocks.
	Modules map[string]Module `yaml:"modules"`
	// Catchall lists the unannotated addresses that defaulted to the remainder.
	Catchall []string `yaml:"catchall,omitempty"`
	// DuplicatedData maps a data source to the modules holding a copy, derived
	// from where its consumers landed.
	DuplicatedData map[string][]string `yaml:"duplicated_data,omitempty"`
	// StateMoves lists the managed addresses migrate must relocate. Remainder
	// resources are absent: the carved-down monolith state becomes its state.
	StateMoves []StateMove `yaml:"state_moves"`
	// CrossEdges is the value wiring a control plane needs at adoption time.
	CrossEdges []CrossEdge `yaml:"cross_edges,omitempty"`
	// OrderingEdges are whole-module apply-order dependencies with no value.
	OrderingEdges []OrderingEdge `yaml:"ordering_edges,omitempty"`
	// EmitChecksum is a hash over the emitted roots, for the staleness guard.
	EmitChecksum string `yaml:"emit_checksum"`
}

// Source identifies the monolith the manifest was computed from.
type Source struct {
	Root            string `yaml:"root"`
	RemainderModule string `yaml:"remainder_module"`
}

// Output records where the carved roots were emitted, relative to the root dir.
type Output struct {
	Dir string `yaml:"dir"`
}

// Module is one carved root.
type Module struct {
	// Dir is the module's emitted root, relative to the monolith root dir.
	Dir string `yaml:"dir"`
	// Blocks are the assigned addresses (resource, data.*, module.*).
	Blocks []string `yaml:"blocks"`
}

// StateMove relocates one managed address into a module's state.
type StateMove struct {
	Address string `yaml:"address"`
	Module  string `yaml:"module"`
}

// CrossEdge is a value-carrying boundary crossing: the producer module exposes
// Output, threaded into the consumer module's Input.
type CrossEdge struct {
	ProducerModule string `yaml:"producer_module"`
	Producer       string `yaml:"producer"`
	Attribute      string `yaml:"attribute,omitempty"`
	Output         string `yaml:"output"`
	ConsumerModule string `yaml:"consumer_module"`
	Consumer       string `yaml:"consumer"`
	Input          string `yaml:"input"`
}

// OrderingEdge is a whole-module apply-order dependency (cross-module
// depends_on); no variable/output is involved.
type OrderingEdge struct {
	ConsumerModule string `yaml:"consumer_module"`
	ProducerModule string `yaml:"producer_module"`
	Consumer       string `yaml:"consumer"`
	Producer       string `yaml:"producer"`
}

// Path is the canonical manifest location for a root.
func Path(rootDir string) string {
	return filepath.Join(rootDir, FileName)
}

// Build assembles a manifest from an analysis and the emitted roots. rootDir is
// the monolith root the manifest will live in; module dirs are stored relative
// to it.
func Build(a *pipeline.Analysis, rootDir, outDir string, ems []emit.EmittedModule, created time.Time, tool string) (*Manifest, error) {
	m := FromAnalysis(a)
	m.Created = created.UTC().Format(time.RFC3339)
	m.Tool = tool
	m.Source.Root = rootDir
	m.Output = Output{Dir: relTo(rootDir, outDir)}

	dirs := map[string]string{}
	for _, em := range ems {
		dirs[em.Module] = em.Dir
	}
	for name, mod := range m.Modules {
		mod.Dir = relTo(rootDir, dirs[name])
		m.Modules[name] = mod
	}

	sum, err := Checksum(m.ModuleDirs(rootDir))
	if err != nil {
		return nil, err
	}
	m.EmitChecksum = sum
	return m, nil
}

// FromAnalysis derives the manifest's semantic content (modules, moves, edges)
// from an analysis, with no paths, checksum, or provenance. diff and prove
// compare this against a committed manifest via SemanticEqual.
func FromAnalysis(a *pipeline.Analysis) *Manifest {
	m := &Manifest{
		Version: SchemaVersion,
		Source:  Source{RemainderModule: a.Placement.Remainder},
		Modules: map[string]Module{},
	}

	for _, name := range a.Placement.ModuleNames() {
		blocks := make([]string, 0, len(a.Placement.Modules[name]))
		for _, addr := range a.Placement.Modules[name] {
			blocks = append(blocks, addr.String())
		}
		m.Modules[name] = Module{Blocks: blocks}
	}

	for _, addr := range a.Placement.Catchall {
		m.Catchall = append(m.Catchall, addr.String())
	}
	if len(a.Placement.Duplicated) > 0 {
		m.DuplicatedData = map[string][]string{}
		for addr, mods := range a.Placement.Duplicated {
			sorted := append([]string(nil), mods...)
			sort.Strings(sorted)
			m.DuplicatedData[addr] = sorted
		}
	}

	plan := statemove.BuildPlan(a.Placement)
	moduleNames := make([]string, 0, len(plan.Moves))
	for name := range plan.Moves {
		moduleNames = append(moduleNames, name)
	}
	sort.Strings(moduleNames)
	for _, name := range moduleNames {
		if name == plan.Remainder {
			continue
		}
		for _, mv := range plan.Moves[name] {
			m.StateMoves = append(m.StateMoves, StateMove{Address: mv.SourceAddr, Module: name})
		}
	}

	for _, e := range a.Boundary.CrossEdges {
		attr := ""
		if pb := a.Boundary.Boundaries[e.ProducerModule]; pb != nil {
			if o, ok := pb.Outputs[e.OutputName]; ok {
				attr = o.Attr
			}
		}
		m.CrossEdges = append(m.CrossEdges, CrossEdge{
			ProducerModule: e.ProducerModule,
			Producer:       e.Producer.String(),
			Attribute:      attr,
			Output:         e.OutputName,
			ConsumerModule: e.ConsumerModule,
			Consumer:       e.Consumer.String(),
			Input:          e.InputName,
		})
	}
	for _, e := range a.Boundary.OrderingEdges {
		m.OrderingEdges = append(m.OrderingEdges, OrderingEdge{
			ConsumerModule: e.ConsumerModule,
			ProducerModule: e.ProducerModule,
			Consumer:       e.Consumer.String(),
			Producer:       e.Producer.String(),
		})
	}
	return m
}

// ModuleDirs resolves every module's emitted dir against rootDir.
func (m *Manifest) ModuleDirs(rootDir string) map[string]string {
	out := map[string]string{}
	for name, mod := range m.Modules {
		out[name] = Resolve(rootDir, mod.Dir)
	}
	return out
}

// OutDir resolves the output dir against rootDir.
func (m *Manifest) OutDir(rootDir string) string {
	return Resolve(rootDir, m.Output.Dir)
}

// RemainderIsStateful reports whether the remainder module holds any block that
// carries state (a managed resource or module call), i.e. whether migrate must
// adopt the carved-down monolith state as the remainder's state.
func (m *Manifest) RemainderIsStateful() bool {
	rem, ok := m.Modules[m.Source.RemainderModule]
	if !ok {
		return false
	}
	for _, b := range rem.Blocks {
		if !strings.HasPrefix(b, "data.") {
			return true
		}
	}
	return false
}

// Plan reconstructs the state-move plan migrate executes.
func (m *Manifest) Plan() *statemove.Plan {
	p := &statemove.Plan{
		Moves:          map[string][]statemove.Move{},
		Remainder:      m.Source.RemainderModule,
		AdoptRemainder: m.RemainderIsStateful(),
	}
	for _, mv := range m.StateMoves {
		p.Moves[mv.Module] = append(p.Moves[mv.Module], statemove.Move{SourceAddr: mv.Address, DestAddr: mv.Address})
	}
	return p
}

// Write marshals the manifest to path.
func Write(m *Manifest, path string) error {
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// Load reads and version-checks a manifest.
func Load(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if m.Version > SchemaVersion {
		return nil, fmt.Errorf("manifest %s has schema version %d; this build supports up to %d", path, m.Version, SchemaVersion)
	}
	if m.Version < 1 {
		return nil, fmt.Errorf("manifest %s has no schema version", path)
	}
	return &m, nil
}

// Resolve joins a manifest-stored path with rootDir unless already absolute.
func Resolve(rootDir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(rootDir, p)
}

// relTo stores p relative to rootDir when possible, keeping manifests portable
// across checkouts.
func relTo(rootDir, p string) string {
	if p == "" {
		return p
	}
	absRoot, err1 := filepath.Abs(rootDir)
	absP, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return p
	}
	rel, err := filepath.Rel(absRoot, absP)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p
	}
	return rel
}

// checksumSkip lists artifacts later stages write into module dirs; excluding
// them keeps the checksum a function of the emitted code alone.
func checksumSkip(name string) bool {
	if name == ".terraform" || name == ".terraform.lock.hcl" {
		return true
	}
	if name == "demono.tfplan" || name == "generated.auto.tfvars" {
		return true
	}
	if strings.Contains(name, ".tfstate") || strings.HasSuffix(name, ".backup") {
		return true
	}
	return false
}

// Checksum hashes the emitted module roots: every file's module, relative path,
// and content, in sorted order, excluding artifacts later stages write.
func Checksum(moduleDirs map[string]string) (string, error) {
	files, err := FileHashes(moduleDirs)
	if err != nil {
		return "", err
	}
	return ChecksumOf(files), nil
}

// ChecksumOf folds a FileHashes map into the single emit checksum.
func ChecksumOf(files map[string]string) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%s\x00", k, files[k])
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

// FileHashes maps "<module>/<relpath>" to a content hash for every emitted
// file, using the same skip rules as Checksum. Used by diff to name
// the files that differ.
func FileHashes(moduleDirs map[string]string) (map[string]string, error) {
	out := map[string]string{}
	names := make([]string, 0, len(moduleDirs))
	for n := range moduleDirs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		dir := moduleDirs[name]
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if checksumSkip(info.Name()) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if info.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[name+"/"+filepath.ToSlash(rel)] = fmt.Sprintf("%x", sha256.Sum256(b))
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("hash module %s: %w", name, err)
		}
	}
	return out, nil
}

// SemanticEqual compares the analysis-derived content of two manifests,
// ignoring provenance (created, tool, paths, checksum). diff uses it
// to prove a committed manifest matches what the committed source produces.
func SemanticEqual(a, b *Manifest) bool {
	if a.Source.RemainderModule != b.Source.RemainderModule {
		return false
	}
	if len(a.Modules) != len(b.Modules) {
		return false
	}
	for name, am := range a.Modules {
		bm, ok := b.Modules[name]
		if !ok || !equalStrings(am.Blocks, bm.Blocks) {
			return false
		}
	}
	if !equalStrings(a.Catchall, b.Catchall) {
		return false
	}
	if len(a.DuplicatedData) != len(b.DuplicatedData) {
		return false
	}
	for k, av := range a.DuplicatedData {
		if !equalStrings(av, b.DuplicatedData[k]) {
			return false
		}
	}
	if len(a.StateMoves) != len(b.StateMoves) || len(a.CrossEdges) != len(b.CrossEdges) || len(a.OrderingEdges) != len(b.OrderingEdges) {
		return false
	}
	for i := range a.StateMoves {
		if a.StateMoves[i] != b.StateMoves[i] {
			return false
		}
	}
	for i := range a.CrossEdges {
		if a.CrossEdges[i] != b.CrossEdges[i] {
			return false
		}
	}
	for i := range a.OrderingEdges {
		if a.OrderingEdges[i] != b.OrderingEdges[i] {
			return false
		}
	}
	return true
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
