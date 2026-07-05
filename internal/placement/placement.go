// Package placement resolves decorators into a total assignment of graph nodes
// to modules. Every managed resource and data source lands in exactly one
// module — except a multi-target data source, which is duplicated into each of
// its targets. Unannotated resources/data fall into the catchall (remainder)
// module, guaranteeing a total assignment with nothing left unplaced.
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
	// into (len >= 2).
	Duplicated map[string][]string
	// Catchall is the list of node addresses that landed in the remainder
	// module because they were unannotated, reported each run.
	Catchall []hclgraph.Address
}

// Options configures placement resolution.
type Options struct {
	// Remainder is the catchall module name (default "monolith").
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
		remainder = "monolith"
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
			targets := decoByAddr[node.Addr.String()]
			switch {
			case len(targets) == 0:
				p.assign(remainder, node.Addr)
				p.Owner[node.Addr.String()] = remainder
				p.Catchall = append(p.Catchall, node.Addr)
			case len(targets) == 1:
				p.assign(targets[0], node.Addr)
				p.Owner[node.Addr.String()] = targets[0]
			default:
				// Multi-target data source: duplicate into each target.
				for _, m := range targets {
					p.assign(m, node.Addr)
				}
				p.Duplicated[node.Addr.String()] = targets
			}
		default:
			// var/local/output/module: not directly placed.
		}
	}

	for m := range p.Modules {
		sortAddrs(p.Modules[m])
	}
	sortAddrs(p.Catchall)
	return p, nil
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
