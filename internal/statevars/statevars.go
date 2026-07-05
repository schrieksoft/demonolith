// Package statevars materializes per-module input values by reading the applied
// monolith state and resolving every cross-module boundary input to the concrete
// attribute value of its producing resource. It writes each consumer module a
// `generated.auto.tfvars` file so the carved root is self-contained and provable
// standalone: planning it against its carved state with these vars yields
// zero-diff, exactly as it did inside the monolith.
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

// TfvarsName is the fixed filename written into each consumer module. The
// `.auto.tfvars` suffix means Terraform loads it automatically on plan/apply, so
// no -var-file flag is needed.
const TfvarsName = "generated.auto.tfvars"

// State is the parsed subset of a Terraform state file we resolve values from.
type State struct {
	// values maps a resource address ("type.name" or "data.type.name") to its
	// first instance's flat attribute map.
	values map[string]map[string]any
}

// Result records what Generate wrote.
type Result struct {
	// Files maps a module name to the .tfvars path written for it (only modules
	// that had at least one resolvable cross-module input appear).
	Files map[string]string
	// Values maps module -> input name -> the resolved string value written.
	Values map[string]map[string]string
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

// Options configures Generate.
type Options struct {
	// SourceDir is the monolith root, used to resolve a module-call output to
	// the child resource attribute it exposes. May be empty if no cross-module
	// edge has a module producer.
	SourceDir string
}

// Generate resolves every cross-module input from state and writes a
// generated.auto.tfvars into each consumer module directory. moduleDirs maps a
// module name to its carved root directory. An input whose producer attribute
// cannot be resolved from state is a hard error: it means the split would
// produce a module that cannot plan.
func Generate(st *State, bound *boundary.Result, moduleDirs map[string]string, opts Options) (*Result, error) {
	res := &Result{Files: map[string]string{}, Values: map[string]map[string]string{}}

	for _, edge := range bound.CrossEdges {
		attr := ""
		if pb := bound.Boundaries[edge.ProducerModule]; pb != nil {
			if o, ok := pb.Outputs[edge.OutputName]; ok {
				attr = o.Attr
			}
		}

		v, ok := st.resolveProducer(edge.Producer, attr, opts.SourceDir)
		if !ok {
			return nil, fmt.Errorf("module %q input %q: cannot resolve %s.%s from state (is the monolith applied?)",
				edge.ConsumerModule, edge.InputName, edge.Producer, attr)
		}
		if res.Values[edge.ConsumerModule] == nil {
			res.Values[edge.ConsumerModule] = map[string]string{}
		}
		res.Values[edge.ConsumerModule][edge.InputName] = stringify(v)
	}

	// Write one tfvars file per consumer module that has resolved inputs.
	modules := make([]string, 0, len(res.Values))
	for m := range res.Values {
		modules = append(modules, m)
	}
	sort.Strings(modules)

	for _, module := range modules {
		dir, ok := moduleDirs[module]
		if !ok {
			return nil, fmt.Errorf("no output directory for module %q", module)
		}
		f := hclwrite.NewEmptyFile()
		body := f.Body()
		names := make([]string, 0, len(res.Values[module]))
		for n := range res.Values[module] {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			// Every generated variable is typed string (see emit.writeVariable),
			// so values are written as string literals — matching Snap CD's
			// stringified passing.
			body.SetAttributeValue(n, cty.StringVal(res.Values[module][n]))
		}
		path := filepath.Join(dir, TfvarsName)
		if err := os.WriteFile(path, f.Bytes(), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
		res.Files[module] = path
	}
	return res, nil
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
