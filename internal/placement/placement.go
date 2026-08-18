// Package placement resolves decorators into a total assignment of graph nodes
// to modules. Managed resources and module calls are placed by decorator, with
// unannotated ones falling into the catchall (remainder) module. Data sources
// are never decorated: a data source is a stateless read and follows its
// consumers automatically — copied into every module that references it,
// directly or through locals or other data sources. The result is a total
// assignment with nothing left unplaced.
package placement

import (
	"fmt"
	"sort"

	"github.com/schrieksoft/demonolith/internal/decorator"
	"github.com/schrieksoft/demonolith/internal/hclgraph"
)

// Placement is the desired assignment of nodes to modules.
type Placement struct {
	// Remainder is the catchall module name.
	Remainder string
	// Module lists the (possibly duplicated) node addresses assigned to each
	// module, sorted. Keyed by module name.
	Modules map[string][]hclgraph.Address
	// Owner maps a node address to the single module that owns it. For a
	// duplicated data source there is no single owner, so it is absent here and
	// present in Duplicated instead.
	Owner map[string]string
	// Duplicated maps a data-source address to the set of modules it was copied
	// into (len >= 2), derived from where its consumers landed.
	Duplicated map[string][]string
	// Catchall is the list of node addresses that landed in the remainder
	// module by default — unannotated resources/modules, and data sources with
	// no placed consumer — reported each run.
	Catchall []hclgraph.Address
}

// Options configures placement resolution.
type Options struct {
	// Remainder is the catchall module name (default "legacy").
	Remainder string
}

// Resolve builds the desired placement from the graph and the per-block
// decorators (already validated for arity by the decorator package).
//
// var/local/output/module-call nodes are structural, not placed directly: a
// variable/local is materialized wherever its consumers live, an output is
// generated at a module boundary. So only resource and data nodes are assigned.
func Resolve(g *hclgraph.Graph, decos []decorator.BlockDecorators, opts Options) (*Placement, error) {
	remainder := opts.Remainder
	if remainder == "" {
		remainder = "legacy"
	}

	// Index decorators by the address they attach to.
	decoByAddr := map[string][]string{} // addr -> target modules (flattened)
	for _, bd := range decos {
		var targets []string
		for _, d := range bd.Decorators {
			targets = append(targets, d.Targets...)
		}
		if len(targets) > 0 {
			decoByAddr[bd.Addr] = dedupStrings(targets)
		}
	}

	p := &Placement{
		Remainder:  remainder,
		Modules:    map[string][]hclgraph.Address{},
		Owner:      map[string]string{},
		Duplicated: map[string][]string{},
	}

	// Pass 1: stateful blocks (resources, module calls) are placed by decorator.
	for _, node := range g.SortedNodes() {
		switch node.Addr.Kind {
		case hclgraph.KindResource, hclgraph.KindModule:
			// A managed resource or a module call is a stateful singleton: exactly
			// one target, or the catchall if undecorated.
			targets := decoByAddr[node.Addr.String()]
			mod := remainder
			if len(targets) == 1 {
				mod = targets[0]
			} else if len(targets) > 1 {
				// Should have been caught by decorator arity validation.
				return nil, fmt.Errorf("%s has %d targets; expected one", node.Addr, len(targets))
			} else {
				p.Catchall = append(p.Catchall, node.Addr)
			}
			p.assign(mod, node.Addr)
			p.Owner[node.Addr.String()] = mod

		case hclgraph.KindData:
			if len(decoByAddr[node.Addr.String()]) > 0 {
				// Should have been caught by decorator arity validation.
				return nil, fmt.Errorf("%s carries a decorator; data sources are placed automatically wherever they are referenced", node.Addr)
			}
		default:
			// var/local/output: not directly placed.
		}
	}

	// Pass 2: data sources follow their consumers — copied into every module
	// that references them, directly or through locals or other data sources.
	needs := newNeedsIndex(g, p.Owner)
	for _, node := range g.SortedNodes() {
		if node.Addr.Kind != hclgraph.KindData {
			continue
		}
		mods := needs.modulesFor(node.Addr.String())
		switch len(mods) {
		case 0:
			// No placed consumer: default to the remainder, reported.
			p.assign(remainder, node.Addr)
			p.Owner[node.Addr.String()] = remainder
			p.Catchall = append(p.Catchall, node.Addr)
		case 1:
			p.assign(mods[0], node.Addr)
			p.Owner[node.Addr.String()] = mods[0]
		default:
			for _, m := range mods {
				p.assign(m, node.Addr)
			}
			p.Duplicated[node.Addr.String()] = mods
		}
	}

	for m := range p.Modules {
		sortAddrs(p.Modules[m])
	}
	sortAddrs(p.Catchall)
	return p, nil
}

// needsIndex answers, for a data source, the set of modules whose placed
// blocks consume it. Consumption is transitive through structural nodes: a
// local that wraps a data result is materialized wherever its consumers live,
// so the data source must exist there too; likewise a data source whose
// argument reads another data source drags that one along.
type needsIndex struct {
	// consumers maps a producer address to the nodes whose value refs mention
	// it (DependsOnOnly refs excluded: ordering needs no copy).
	consumers map[string][]*hclgraph.Node
	owner     map[string]string
	cache     map[string][]string
}

func newNeedsIndex(g *hclgraph.Graph, owner map[string]string) *needsIndex {
	idx := &needsIndex{consumers: map[string][]*hclgraph.Node{}, owner: owner, cache: map[string][]string{}}
	for _, n := range g.SortedNodes() {
		for _, ref := range n.Refs {
			key := ref.String()
			idx.consumers[key] = append(idx.consumers[key], n)
		}
	}
	return idx
}

// modulesFor returns the sorted module set needing the producer at key.
func (idx *needsIndex) modulesFor(key string) []string {
	return idx.walk(key, map[string]bool{})
}

func (idx *needsIndex) walk(key string, visiting map[string]bool) []string {
	if got, ok := idx.cache[key]; ok {
		return got
	}
	if visiting[key] {
		return nil
	}
	visiting[key] = true
	defer delete(visiting, key)

	set := map[string]bool{}
	for _, c := range idx.consumers[key] {
		switch c.Addr.Kind {
		case hclgraph.KindResource, hclgraph.KindModule:
			set[idx.owner[c.Addr.String()]] = true
		case hclgraph.KindLocal, hclgraph.KindData:
			for _, m := range idx.walk(c.Addr.String(), visiting) {
				set[m] = true
			}
		default:
			// var/output consumers pin nothing to a module.
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	idx.cache[key] = out
	return out
}

// ModuleNames returns the module names in sorted order, including the remainder
// module only if it is non-empty.
func (p *Placement) ModuleNames() []string {
	names := make([]string, 0, len(p.Modules))
	for m, addrs := range p.Modules {
		if m == p.Remainder && len(addrs) == 0 {
			continue
		}
		names = append(names, m)
	}
	sort.Strings(names)
	return names
}

// ModuleOf returns the owning module for a non-duplicated address. For a
// duplicated data source, ok is false (it has multiple homes).
func (p *Placement) ModuleOf(addr hclgraph.Address) (string, bool) {
	if _, dup := p.Duplicated[addr.String()]; dup {
		return "", false
	}
	m, ok := p.Owner[addr.String()]
	return m, ok
}

// ModulesOf returns every module an address lives in (one, or many if
// duplicated).
func (p *Placement) ModulesOf(addr hclgraph.Address) []string {
	if mods, dup := p.Duplicated[addr.String()]; dup {
		return mods
	}
	if m, ok := p.Owner[addr.String()]; ok {
		return []string{m}
	}
	return nil
}

func (p *Placement) assign(module string, addr hclgraph.Address) {
	p.Modules[module] = append(p.Modules[module], addr)
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func sortAddrs(a []hclgraph.Address) {
	sort.Slice(a, func(i, j int) bool { return a[i].String() < a[j].String() })
}
