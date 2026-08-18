// Package statevars materializes per-module input values into two files per
// carved root: `demono.root.tfvars` (the root variable values the module
// declares, resolved as the monolith resolved them; written at prove) and
// `demono.graph.tfvars` (the cross-module boundary inputs resolved from
// the applied monolith state; written at run). Together they make a carved
// root self-contained and provable standalone: planning it against its carved
// state with these vars yields zero-diff, exactly as it did inside the
// monolith.
//
// This is the file-materialized counterpart to the proof oracle's in-memory
// threading. The oracle threads a producer's *planned* output; statevars reads
// the producer's *applied* state directly — the value already committed to
// infrastructure — which is what makes the split provable before Snap CD is
// wired in.
package statevars

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/schrieksoft/demonolith/internal/boundary"
	"github.com/schrieksoft/demonolith/internal/hclgraph"
)

// The fixed filenames written into each carved module. Deliberately NOT
// `.auto.tfvars`: loading is always explicit (-var-file), by demonolith's
// proofs and by anyone planning a root standalone.
const (
	// RootTfvarsName holds the module's root variable values (written at
	// prove).
	RootTfvarsName = "demono.root.tfvars"
	// GraphTfvarsName holds the module's cross-module input values (written
	// at run).
	GraphTfvarsName = "demono.graph.tfvars"
)

// State is the parsed subset of a Terraform state file we resolve values from.
type State struct {
	// values maps a resource address ("type.name" or "data.type.name") to its
	// first instance's flat attribute map.
	values map[string]map[string]any
}

// Result records what Write wrote.
type Result struct {
	// Files maps a module name to the .tfvars path written for it (only modules
	// that had at least one value appear).
	Files map[string]string
	// Values maps module -> input name -> the resolved string value written.
	Values map[string]map[string]string
	// Unresolved lists cross-module inputs that could not be resolved from
	// state and were left out (threaded from producer plans instead).
	Unresolved []string
}

// LoadState reads and parses a Terraform state file.
func LoadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	var raw struct {
		Resources []struct {
			Module    string `json:"module"` // e.g. "module.idgen"; "" for root
			Mode      string `json:"mode"`
			Type      string `json:"type"`
			Name      string `json:"name"`
			Instances []struct {
				Attributes map[string]any `json:"attributes"`
			} `json:"instances"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	st := &State{values: map[string]map[string]any{}}
	for _, r := range raw.Resources {
		if len(r.Instances) == 0 {
			continue
		}
		addr := r.Type + "." + r.Name
		if r.Mode == "data" {
			addr = "data." + addr
		}
		// Module-scoped resources are keyed by their full state address
		// (module.<name>.<type>.<name>) so a module output can be resolved.
		if r.Module != "" {
			addr = r.Module + "." + addr
		}
		// v1 addresses have no instance key; use the first (only) instance.
		st.values[addr] = r.Instances[0].Attributes
	}
	return st, nil
}

// resolveProducer resolves a cross-module producer's value from state.
// Resource/data producers are read directly. A module-call producer's output
// (module.<name>.<output>) is resolved by parsing the child module's `output`
// block to find which child resource attribute it exposes, then reading that
// attribute from the module-scoped resource in state.
func (s *State) resolveProducer(addr hclgraph.Address, attr, sourceDir string) (any, bool) {
	if addr.Kind != hclgraph.KindModule {
		return s.lookup(addr, attr)
	}
	// attr is the module output name (e.g. "id"). Find the child resource+attr
	// it exposes.
	childRes, childAttr, ok := moduleOutputTarget(sourceDir, addr.Name, attr)
	if !ok {
		return nil, false
	}
	// State key for a module-scoped resource: module.<name>.<type>.<name>
	key := addr.String() + "." + childRes
	attrs, ok := s.values[key]
	if !ok {
		return nil, false
	}
	if childAttr == "" {
		return attrs, true
	}
	return walkAttr(attrs, splitPath(childAttr))
}

// lookup resolves a producer address + attribute path (e.g. "result", or a
// dotted path into a nested attribute) to a concrete value from state.
func (s *State) lookup(addr hclgraph.Address, attr string) (any, bool) {
	attrs, ok := s.values[addr.String()]
	if !ok {
		return nil, false
	}
	if attr == "" {
		return attrs, true
	}
	return walkAttr(attrs, splitPath(attr))
}

// walkAttr descends a dotted attribute path through nested maps.
func walkAttr(cur any, path []string) (any, bool) {
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func splitPath(attr string) []string {
	var out []string
	cur := ""
	for _, r := range attr {
		if r == '.' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// Options configures ResolveCross.
type Options struct {
	// SourceDir is the monolith root, used to resolve a module-call output to
	// the child resource attribute it exposes. May be empty if no cross-module
	// edge has a module producer.
	SourceDir string
}

// Unresolved identifies a cross-module input whose value could not be
// resolved from state.
type Unresolved struct {
	Consumer string
	Input    string
	Producer string
	Attr     string
}

func (u Unresolved) String() string {
	return fmt.Sprintf("%s: %s (%s.%s)", u.Consumer, u.Input, u.Producer, u.Attr)
}

// ResolveCross resolves every cross-module input from state. An input whose
// producer attribute cannot be resolved offline — a computed value, or a
// module call whose source is not on disk — is listed in unresolved and left
// out of the values: migrate run fills those from the proof's threaded
// producer outputs, and a control plane supplies them at runtime.
func ResolveCross(st *State, bound *boundary.Result, opts Options) (map[string]map[string]string, []Unresolved) {
	vals := map[string]map[string]string{}
	var unresolved []Unresolved
	for _, edge := range bound.CrossEdges {
		attr := ""
		if pb := bound.Boundaries[edge.ProducerModule]; pb != nil {
			if o, ok := pb.Outputs[edge.OutputName]; ok {
				attr = o.Attr
			}
		}

		v, ok := st.resolveProducer(edge.Producer, attr, opts.SourceDir)
		if !ok {
			unresolved = append(unresolved, Unresolved{Consumer: edge.ConsumerModule, Input: edge.InputName, Producer: edge.Producer.String(), Attr: attr})
			continue
		}
		if vals[edge.ConsumerModule] == nil {
			vals[edge.ConsumerModule] = map[string]string{}
		}
		vals[edge.ConsumerModule][edge.InputName] = stringify(v)
	}
	sort.Slice(unresolved, func(i, j int) bool {
		if unresolved[i].Consumer != unresolved[j].Consumer {
			return unresolved[i].Consumer < unresolved[j].Consumer
		}
		return unresolved[i].Input < unresolved[j].Input
	})
	return vals, unresolved
}

// Collect merges root and cross-module values per module without writing any
// file — the in-memory counterpart of Write for --no-tfvars runs.
func Collect(rootVals, crossVals map[string]map[string]string) *Result {
	res := &Result{Files: map[string]string{}, Values: map[string]map[string]string{}}
	for _, vals := range []map[string]map[string]string{rootVals, crossVals} {
		for module, kv := range vals {
			if res.Values[module] == nil {
				res.Values[module] = map[string]string{}
			}
			for k, v := range kv {
				res.Values[module][k] = v
			}
		}
	}
	return res
}

// WriteRoot materializes each module's demono.root.tfvars — the root
// variable values it declares, resolved as the monolith resolved them.
// Modules with no values get no file.
func WriteRoot(moduleDirs map[string]string, vals map[string]map[string]string) (*Result, error) {
	return writeTfvars(moduleDirs, vals, RootTfvarsName,
		"# Root variable values, resolved as the monolith resolved them.\n")
}

// WriteGraph materializes each module's demono.graph.tfvars — its
// cross-module input values resolved from the applied monolith state.
// Modules with no values get no file.
func WriteGraph(moduleDirs map[string]string, vals map[string]map[string]string) (*Result, error) {
	return writeTfvars(moduleDirs, vals, GraphTfvarsName,
		"# Cross-module input values, resolved from the applied monolith state or\n# filled in from the producer values the proof computed.\n")
}

func writeTfvars(moduleDirs map[string]string, vals map[string]map[string]string, filename, header string) (*Result, error) {
	res := &Result{Files: map[string]string{}, Values: map[string]map[string]string{}}
	names := make([]string, 0, len(vals))
	for m := range vals {
		names = append(names, m)
	}
	sort.Strings(names)

	for _, module := range names {
		if len(vals[module]) == 0 {
			continue
		}
		dir, ok := moduleDirs[module]
		if !ok {
			return nil, fmt.Errorf("no output directory for module %q", module)
		}
		content := append([]byte(header), encodeAssignments(vals[module])...)
		path := filepath.Join(dir, filename)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
		res.Files[module] = path
		res.Values[module] = vals[module]
	}
	return res, nil
}

// encodeAssignments renders name = "value" lines in sorted order. Every
// generated variable is typed string (see emit.writeVariable), so values are
// written as string literals — matching Snap CD's stringified passing.
func encodeAssignments(vals map[string]string) []byte {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	names := make([]string, 0, len(vals))
	for n := range vals {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		body.SetAttributeValue(n, cty.StringVal(vals[n]))
	}
	return f.Bytes()
}

// stringify renders a state attribute value the way Snap CD passes values
// downstream: scalars bare, composites as compact JSON. Mirrors
// proof.stringify so file-materialized values match threaded ones.
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case json.Number:
		return t.String()
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}
