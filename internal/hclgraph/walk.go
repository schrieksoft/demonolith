package hclgraph

import (
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// walkBody traverses every attribute expression and nested block in a body,
// recording references into seen. Nested blocks (dynamic, provisioner, nested
// resource blocks) are walked recursively so refs there are not missed.
func (g *Graph) walkBody(body *hclsyntax.Body, c *refCollector) {
	for name, attr := range body.Attributes {
		if name == "depends_on" {
			// depends_on refs are ordering-only: collected separately so a
			// producer referenced *only* here does not induce a value input.
			dep := newRefCollector()
			g.walkExpr(attr.Expr, dep)
			for k, addr := range dep.seen {
				c.dependsOn[k] = addr
			}
			continue
		}
		if name == "provider" {
			// The provider meta-argument (`provider = name.alias`) selects an
			// aliased provider instance; it is not a value reference. Capture the
			// traversal segments so emit can carve the right aliased block.
			if te, ok := attr.Expr.(*hclsyntax.ScopeTraversalExpr); ok {
				c.providerRef = traversalSegments(te.Traversal)
			}
			continue
		}
		g.walkExpr(attr.Expr, c)
	}
	for _, blk := range body.Blocks {
		g.walkBody(blk.Body, c)
	}
}

// walkExpr extracts references from a single expression. We use hclsyntax.Walk
// to visit every sub-expression node and pull traversals from each, rather than
// relying on Expression.Variables(). Variables() is known to miss traversals
// that appear as the target of an index expression (e.g. foo.x[count.index].y),
// because it only returns "absolute" traversals. Walking the AST and collecting
// from every ScopeTraversalExpr and RelativeTraversalExpr closes that gap.
func (g *Graph) walkExpr(expr hclsyntax.Expression, c *refCollector) {
	if expr == nil {
		return
	}
	// Our walkFn.Enter/Exit never emit diagnostics, so Walk cannot return any;
	// the return is ignored deliberately.
	_ = hclsyntax.Walk(expr, walkFn(func(node hclsyntax.Node) {
		switch e := node.(type) {
		case *hclsyntax.ScopeTraversalExpr:
			g.recordTraversal(e.Traversal, c)
		case *hclsyntax.RelativeTraversalExpr:
			// The relative part hangs off a source expression (often another
			// traversal) which Walk visits separately; record any absolute
			// segments present on the relative traversal itself.
			g.recordTraversal(e.Traversal, c)
		}
	}))
}

// recordTraversal converts a traversal to a segment list, resolves its root to
// a node address, and records the edge (and the referenced attribute path) if
// it targets a known node.
func (g *Graph) recordTraversal(tr hcl.Traversal, c *refCollector) {
	segs := traversalSegments(tr)
	addr, ok := ParseRefRoot(segs)
	if !ok {
		return
	}
	// Only record edges to nodes that actually exist in this root. This filters
	// meta-refs the heuristic let through and references to provider/builtin
	// symbols. var/local/module targets are trusted even if the definition is
	// in another file already collected; resources/data must exist.
	switch addr.Kind {
	case KindResource, KindData:
		if _, exists := g.Nodes[addr.String()]; !exists {
			return
		}
	case KindVariable, KindLocal, KindModule:
		if _, exists := g.Nodes[addr.String()]; !exists {
			// A var/local/module reference to something undefined is a config
			// error; skip rather than fabricate a node.
			return
		}
	}
	key := addr.String()
	c.seen[key] = addr

	// Capture the attribute path following the node prefix, for resource/data/
	// module producers, so an emitted output can expose the right attribute
	// (e.g. module.idgen.id -> "id"). Every distinct path is recorded: a
	// consumer may use several attributes of one producer, and each needs its
	// own output.
	if addr.Kind == KindResource || addr.Kind == KindData || addr.Kind == KindModule {
		prefixLen := len(addr.refPrefix())
		attr := ""
		if len(segs) > prefixLen {
			attr = strings.Join(segs[prefixLen:], ".")
		}
		found := false
		for _, a := range c.attrs[key] {
			if a == attr {
				found = true
				break
			}
		}
		if !found {
			c.attrs[key] = append(c.attrs[key], attr)
		}
	}
}

// traversalSegments flattens a traversal into its leading string segments,
// stopping at the first non-name step (index into a variable etc. still yields
// the name prefix, which is what we need to identify the target node).
func traversalSegments(tr hcl.Traversal) []string {
	var segs []string
	for _, step := range tr {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			segs = append(segs, s.Name)
		case hcl.TraverseAttr:
			segs = append(segs, s.Name)
		case hcl.TraverseIndex:
			// stop: the node identity is fixed by the segments before an index
			return segs
		}
	}
	return segs
}

// walkFn adapts a func to the hclsyntax.Walker interface. Enter is where we
// inspect; Exit is a no-op.
type walkFn func(hclsyntax.Node)

func (f walkFn) Enter(node hclsyntax.Node) hcl.Diagnostics { f(node); return nil }
func (f walkFn) Exit(node hclsyntax.Node) hcl.Diagnostics  { return nil }
