package transfer

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The slice is the unit of the migrate half: every step anchors on one root
// and touches only that root's state, exchanging what must cross the boundary
// as artifact files in the slice's workdir. An orchestrator - demonolith's own
// `--all`, or anything external - transports the artifacts between slices.

// Role identifies which part a root plays in the transfer its map describes.
type Role struct {
	// Kind is "source", "receiver", or "snapcd".
	Kind string
	// Key is the map's receiver key (the decorator target) for receivers.
	Key string
	// Base is the root's directory basename - the token artifact filenames use.
	Base string
}

// RoleOf matches a root directory to its part in the map by directory
// basename. Basenames are unique across the transfer (BuildPlan refuses
// collisions), so the match is unambiguous.
func RoleOf(m *Map, dir string) (Role, error) {
	if m.SourceDir == "" {
		return Role{}, fmt.Errorf("the map does not name its source root; re-run `demonolith transfer refactor map` at the source")
	}
	base := filepath.Base(filepath.Clean(dir))
	if base == m.SourceDir {
		return Role{Kind: "source", Base: base}, nil
	}
	for _, name := range m.ReceiverNames() {
		if filepath.Base(name) == base {
			return Role{Kind: "receiver", Key: name, Base: base}, nil
		}
	}
	if m.Snapcd != nil && filepath.Base(m.Snapcd.Dir) == base {
		return Role{Kind: "snapcd", Base: base}, nil
	}
	known := []string{m.SourceDir}
	for _, name := range m.ReceiverNames() {
		known = append(known, filepath.Base(name))
	}
	if m.Snapcd != nil {
		known = append(known, filepath.Base(m.Snapcd.Dir))
	}
	return Role{}, fmt.Errorf("directory %q is not part of this transfer (its map covers: %s)", base, strings.Join(known, ", "))
}

// Module returns the map module name edges use for this role: the remainder
// stands in for the source, receivers go by their map key.
func (r Role) Module(m *Map) string {
	if r.Kind == "source" {
		return m.Remainder
	}
	return r.Key
}

// BaseForModule maps an edge's module name to the artifact-name base.
func BaseForModule(m *Map, module string) string {
	if module == m.Remainder {
		return m.SourceDir
	}
	return filepath.Base(module)
}

// MapHash is the transfer's identity: the sha256 of the map file's bytes.
// Distributed copies are byte-identical, so the hash agrees across roots.
func MapHash(dir string) (string, error) {
	return FileSHA256(filepath.Join(dir, MapFile))
}

// DistributeMap copies the source root's map file verbatim into every given
// directory, so each touched root carries the whole transfer.
func DistributeMap(srcDir string, dirs []string) error {
	b, err := os.ReadFile(filepath.Join(srcDir, MapFile))
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if err := os.WriteFile(filepath.Join(d, MapFile), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// MapCopiesIdentical verifies every distributed copy still matches the
// source's map byte for byte.
func MapCopiesIdentical(srcDir string, dirs map[string]string) error {
	want, err := os.ReadFile(filepath.Join(srcDir, MapFile))
	if err != nil {
		return err
	}
	names := make([]string, 0, len(dirs))
	for n := range dirs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		got, err := os.ReadFile(filepath.Join(dirs[n], MapFile))
		if err != nil {
			return fmt.Errorf("%s carries no %s; re-run `demonolith transfer refactor run` to distribute the map", n, MapFile)
		}
		if !bytes.Equal(want, got) {
			return fmt.Errorf("the %s in %s differs from the source's; the copies must be byte-identical - re-run `demonolith transfer refactor run`", MapFile, n)
		}
	}
	return nil
}

// Workdir artifact names, per slice. The fragment pair travels source →
// receiver, outputs travel producer → consumers, and run receipts travel
// receiver → source.
func SliceStateFile(workDir string) string { return filepath.Join(workDir, "state.tfstate") }
func SlicePostFile(workDir string) string  { return filepath.Join(workDir, "state-post.tfstate") }
func SliceRunFile(workDir string) string   { return filepath.Join(workDir, "state-run.tfstate") }
func SlicePushFile(workDir string) string  { return filepath.Join(workDir, "state-push.tfstate") }
func FragmentStateFile(workDir, base string) string {
	return filepath.Join(workDir, "fragment-"+base+".tfstate")
}
func FragmentMetaFile(workDir, base string) string {
	return filepath.Join(workDir, "fragment-"+base+".yaml")
}
func OutputsFile(workDir, base string) string { return filepath.Join(workDir, "outputs-"+base+".yaml") }
func RecvRunReceiptFile(workDir, base string) string {
	return filepath.Join(workDir, "run-"+base+".yaml")
}

// FragmentMeta identifies a state fragment: which transfer, which receiver,
// extracted from which pinned source state, carrying which addresses.
type FragmentMeta struct {
	Version   int      `yaml:"version"`
	Created   string   `yaml:"created"`
	Tool      string   `yaml:"tool"`
	MapHash   string   `yaml:"map_hash"`
	Receiver  string   `yaml:"receiver"`
	SourcePin Pin      `yaml:"source_pin"`
	Moves     []string `yaml:"moves"`
}

func WriteFragmentMeta(fm *FragmentMeta, workDir string) error {
	b, err := yaml.Marshal(fm)
	if err != nil {
		return err
	}
	return os.WriteFile(FragmentMetaFile(workDir, fm.Receiver), b, 0o644)
}

func LoadFragmentMeta(workDir, base string) (*FragmentMeta, error) {
	b, err := os.ReadFile(FragmentMetaFile(workDir, base))
	if err != nil {
		return nil, err
	}
	var fm FragmentMeta
	if err := yaml.Unmarshal(b, &fm); err != nil {
		return nil, err
	}
	return &fm, nil
}

// OutputsArtifact carries one producer slice's planned output values into its
// consumers' proofs - the file form of the threading Snap CD does at runtime.
type OutputsArtifact struct {
	Version int               `yaml:"version"`
	Created string            `yaml:"created"`
	Tool    string            `yaml:"tool"`
	MapHash string            `yaml:"map_hash"`
	Role    string            `yaml:"role"`
	Outputs map[string]string `yaml:"outputs"`
}

func WriteOutputs(oa *OutputsArtifact, workDir string) error {
	b, err := yaml.Marshal(oa)
	if err != nil {
		return err
	}
	return os.WriteFile(OutputsFile(workDir, oa.Role), b, 0o644)
}

func LoadOutputs(workDir, base string) (*OutputsArtifact, error) {
	b, err := os.ReadFile(OutputsFile(workDir, base))
	if err != nil {
		return nil, err
	}
	var oa OutputsArtifact
	if err := yaml.Unmarshal(b, &oa); err != nil {
		return nil, err
	}
	return &oa, nil
}

// SlicePin is the slice's migrate-map receipt: its own state pinned, tied to
// the transfer by map hash.
type SlicePin struct {
	Version int    `yaml:"version"`
	Created string `yaml:"created"`
	Tool    string `yaml:"tool"`
	MapHash string `yaml:"map_hash"`
	Role    string `yaml:"role"`
	Pin     Pin    `yaml:"pin"`
}

func WriteSlicePin(p *SlicePin, rootDir string) error {
	b, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(rootDir, MigrateMapFile), b, 0o644)
}

func LoadSlicePin(rootDir string) (*SlicePin, error) {
	b, err := os.ReadFile(filepath.Join(rootDir, MigrateMapFile))
	if err != nil {
		return nil, fmt.Errorf("no %s found in %s; run `demonolith transfer migrate map` here first", MigrateMapFile, rootDir)
	}
	var p SlicePin
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// CodeMovedSlice verifies this root's side of the code move: the source no
// longer declares any moved block, a receiver declares all of its own, the
// Snap CD root declares the wiring.
func CodeMovedSlice(dir string, m *Map, role Role) error {
	addrs, err := DirAddrs(dir)
	if err != nil {
		return err
	}
	var problems []string
	switch role.Kind {
	case "source":
		for _, name := range m.ReceiverNames() {
			for _, b := range m.Receivers[name].Blocks {
				if addrs[b] {
					problems = append(problems, fmt.Sprintf("%s still present in the source", b))
				}
			}
		}
	case "receiver":
		for _, b := range m.Receivers[role.Key].Blocks {
			if !addrs[b] {
				problems = append(problems, fmt.Sprintf("%s missing from this receiver", b))
			}
		}
	case "snapcd":
		for _, a := range SnapcdWiringAddrs(m) {
			if !addrs[a] {
				problems = append(problems, fmt.Sprintf("%s missing from the Snap CD root", a))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("the code move has not landed in this root (run `demonolith transfer refactor run` at the source, then bring the change here):\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}
