// Package cycle implements the contraction cycle gate. After placement,
// boundary computation, and data-source duplication, each module is contracted
// to a single node and the boundary-crossing edges are lifted to module-level
// edges. A cycle among modules means the split is impossible: module A needs an
// output of module B while B needs an output of A — illegal in Terraform and
// unresolvable in Snap CD's graph. The gate refuses and reports the named cycle
// path together with the specific crossing references that form it.
package cycle

import (
	"fmt"
	"sort"
	"strings"

	"github.com/schrieksoft/demonolith/internal/boundary"
)

// Cycle is a detected module-level cycle: an ordered list of module names that
// returns to its start, plus the crossing references responsible for each hop.
type Cycle struct {
	Path  []string  // e.g. ["networking", "compute", "networking"]
	Edges []EdgeRef // one per hop, len == len(Path)-1
}

// EdgeRef names a crossing reference that contributes to a cycle hop.
type EdgeRef struct {
	From string // consumer module
	To   string // producer module (dependency)
	// Ref is a human-readable "consumer -> producer" reference.
	Ref string
}

func (c Cycle) Error() string {
	var b strings.Builder
	// Fprintf to a strings.Builder never returns a non-nil error.
	_, _ = fmt.Fprintf(&b, "module dependency cycle: %s\n", strings.Join(c.Path, " → "))
	for _, e := range c.Edges {
		_, _ = fmt.Fprintf(&b, "  %s needs %s via %s\n", e.From, e.To, e.Ref)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Check builds the module dependency graph from cross edges and returns the
// first cycle found, or nil if the split is acyclic. A dependency edge points
// from a consumer module to the producer module it depends on.
func Check(res *boundary.Result) *Cycle {
	// adjacency: consumer -> set of producer modules, with a representative
	// crossing ref per (consumer,producer) pair for diagnostics.
	adj := map[string]map[string]string{}
	addEdge := func(consumer, producer, ref string) {
		if consumer == producer {
			return
		}
		if adj[consumer] == nil {
			adj[consumer] = map[string]string{}
		}
		if _, exists := adj[consumer][producer]; !exists {
			adj[consumer][producer] = ref
		}
	}
	for _, e := range res.CrossEdges {
		addEdge(e.ConsumerModule, e.ProducerModule, fmt.Sprintf("%s → %s", e.Consumer, e.Producer))
	}
	for _, e := range res.OrderingEdges {
		addEdge(e.ConsumerModule, e.ProducerModule, fmt.Sprintf("%s → %s (depends_on)", e.Consumer, e.Producer))
	}

	// Deterministic node ordering.
	var nodes []string
	for m := range res.Boundaries {
		nodes = append(nodes, m)
	}
	sort.Strings(nodes)

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string

	var dfs func(string) *Cycle
	dfs = func(u string) *Cycle {
		color[u] = gray
		stack = append(stack, u)

		succ := make([]string, 0, len(adj[u]))
		for v := range adj[u] {
			succ = append(succ, v)
		}
		sort.Strings(succ)

		for _, v := range succ {
			switch color[v] {
			case gray:
				// Back edge: cycle from v's position on the stack to u, then v.
				return buildCycle(stack, v, adj)
			case white:
				if c := dfs(v); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[u] = black
		return nil
	}

	for _, n := range nodes {
		if color[n] == white {
			if c := dfs(n); c != nil {
				return c
			}
		}
	}
	return nil
}

// buildCycle reconstructs the cycle path from the DFS stack: find start on the
// stack, take the suffix, and close it back to start.
func buildCycle(stack []string, start string, adj map[string]map[string]string) *Cycle {
	idx := -1
	for i, s := range stack {
		if s == start {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	path := append([]string(nil), stack[idx:]...)
	path = append(path, start) // close the loop

	var edges []EdgeRef
	for i := 0; i < len(path)-1; i++ {
		from, to := path[i], path[i+1]
		ref := adj[from][to]
		edges = append(edges, EdgeRef{From: from, To: to, Ref: ref})
	}
	return &Cycle{Path: path, Edges: edges}
}
