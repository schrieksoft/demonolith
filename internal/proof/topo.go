package proof

import (
	"fmt"
	"sort"

	"github.com/schrieksoft/demonolith/internal/boundary"
)

// ModuleDeps returns, per module, the sorted modules it depends on — value
// wiring and ordering edges combined, self-edges dropped.
func ModuleDeps(modules []string, res *boundary.Result) map[string][]string {
	deps := moduleDepSets(modules, res)
	out := make(map[string][]string, len(deps))
	for m, set := range deps {
		names := make([]string, 0, len(set))
		for d := range set {
			names = append(names, d)
		}
		sort.Strings(names)
		out[m] = names
	}
	return out
}

func moduleDepSets(modules []string, res *boundary.Result) map[string]map[string]bool {
	deps := map[string]map[string]bool{}
	for _, m := range modules {
		deps[m] = map[string]bool{}
	}
	add := func(consumer, producer string) {
		if consumer == producer {
			return
		}
		if deps[consumer] == nil {
			deps[consumer] = map[string]bool{}
		}
		deps[consumer][producer] = true
	}
	for _, e := range res.CrossEdges {
		add(e.ConsumerModule, e.ProducerModule)
	}
	for _, e := range res.OrderingEdges {
		add(e.ConsumerModule, e.ProducerModule)
	}
	return deps
}

// TopoOrder returns modules ordered so every producer precedes its consumers,
// using the boundary cross edges (and ordering edges) as producer->consumer
// dependencies. A cycle is impossible after the cycle gate has run, but the
// sort still guards against one defensively.
func TopoOrder(modules []string, res *boundary.Result) ([]string, error) {
	deps := moduleDepSets(modules, res)

	// Kahn's algorithm with deterministic tie-breaking.
	indeg := map[string]int{}
	for _, m := range modules {
		indeg[m] = len(deps[m])
	}
	var ready []string
	for _, m := range modules {
		if indeg[m] == 0 {
			ready = append(ready, m)
		}
	}
	sort.Strings(ready)

	var order []string
	for len(ready) > 0 {
		m := ready[0]
		ready = ready[1:]
		order = append(order, m)
		// Any module depending on m loses a dependency.
		var newly []string
		for _, c := range modules {
			if deps[c][m] {
				indeg[c]--
				if indeg[c] == 0 {
					newly = append(newly, c)
				}
			}
		}
		ready = append(ready, newly...)
		sort.Strings(ready)
	}

	if len(order) != len(modules) {
		return nil, fmt.Errorf("module dependency cycle detected during topo sort (should have been caught by cycle gate)")
	}
	return order, nil
}
