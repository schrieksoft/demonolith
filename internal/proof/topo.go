package proof

import (
	"fmt"
	"sort"

	"github.com/schrieksoft/demonolith/internal/boundary"
)

// topoOrder returns modules ordered so every producer precedes its consumers,
// using the boundary cross edges (and ordering edges) as producer->consumer
// dependencies. A cycle is impossible here because the cycle gate already ran,
// but the sort still guards against one defensively.
func topoOrder(modules []string, res *boundary.Result) ([]string, error) {
	// deps[consumer] = set of producer modules it depends on.
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
