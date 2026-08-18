package hclgraph

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// Node is one top-level configurable object plus the source location of its
// defining block. Refs are the addresses this node references (deduplicated).
type Node struct {
	Addr Address
	// DefRange is the range of the block header, used for diagnostics and to
	// pair a node with a decorator by position.
	DefRange hcl.Range
	// File is the path of the file the block was defined in.
	File string
	// Refs are the addresses referenced anywhere inside this block's body.
	Refs []Address
	// RefAttrs records, for a referenced resource/data/module producer, every
	// distinct attribute path this node uses (e.g. "result" for
	// random_uuid.x.result), in first-seen order. Keyed by producer
	// Address.String(). Used to expose the right attribute(s) in generated
	// outputs. An empty entry means the whole object was referenced.
	RefAttrs map[string][]string
	// DependsOnOnly are producers referenced solely from this node's
	// depends_on (ordering-only, never for value). Across a module boundary
	// these become module ordering edges, not value inputs.
	DependsOnOnly []Address
	// ProviderKey is the explicit provider selected by a `provider = name.alias`
	// meta-argument, in "name" or "name.alias" form. Empty when the block uses
	// the default provider for its type.
	ProviderKey string
}

// Graph is the whole parsed root: every node keyed by address, plus the raw
// hclsyntax bodies retained for callers that need to re-inspect expressions.
type Graph struct {
	Nodes map[string]*Node // key: Address.String()
	// order preserves a stable node ordering for deterministic output.
	order []string
	// Files maps a path to its parsed hclsyntax body.
	Files map[string]*hclsyntax.Body

	// pending* are drained by resolveRefs once all nodes are known.
	pendingBlocks []pendingBlock
	pendingExprs  []pendingExpr
}

// Node returns the node at addr, or nil.
func (g *Graph) Node(addr Address) *Node { return g.Nodes[addr.String()] }

// SortedNodes returns nodes in a stable, deterministic order.
func (g *Graph) SortedNodes() []*Node {
	out := make([]*Node, 0, len(g.order))
	for _, k := range g.order {
		out = append(out, g.Nodes[k])
	}
	return out
}

// ParseDir parses every *.tf file in dir into a Graph. It does not recurse into
// subdirectories (a Terraform root is flat). hclsyntax is used so we can walk
// expression ASTs directly for reference extraction.
func ParseDir(dir string) (*Graph, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	g := &Graph{Nodes: map[string]*Node{}, Files: map[string]*hclsyntax.Body{}}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) == ".tf" {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)

	var diags hcl.Diagnostics
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		f, d := hclsyntax.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
		diags = append(diags, d...)
		if d.HasErrors() {
			continue
		}
		body := f.Body.(*hclsyntax.Body)
		g.Files[path] = body
		if err := g.collectBlocks(path, body); err != nil {
			return nil, err
		}
	}
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse errors: %s", diags.Error())
	}

	// Resolve references now that every node is known, so resource-type
	// heuristics can be validated against real nodes.
	g.resolveRefs()
	return g, nil
}

// collectBlocks turns each recognized top-level block into a Node. locals blocks
// expand into one node per attribute.
func (g *Graph) collectBlocks(path string, body *hclsyntax.Body) error {
	for _, blk := range body.Blocks {
		switch blk.Type {
		case "resource":
			if len(blk.Labels) != 2 {
				return fmt.Errorf("%s: resource block needs 2 labels", blk.TypeRange)
			}
			g.add(&Node{Addr: Address{Kind: KindResource, Type: blk.Labels[0], Name: blk.Labels[1]}, DefRange: blk.DefRange(), File: path}, blk)
		case "data":
			if len(blk.Labels) != 2 {
				return fmt.Errorf("%s: data block needs 2 labels", blk.TypeRange)
			}
			g.add(&Node{Addr: Address{Kind: KindData, Type: blk.Labels[0], Name: blk.Labels[1]}, DefRange: blk.DefRange(), File: path}, blk)
		case "variable":
			if len(blk.Labels) != 1 {
				return fmt.Errorf("%s: variable needs 1 label", blk.TypeRange)
			}
			g.add(&Node{Addr: Address{Kind: KindVariable, Name: blk.Labels[0]}, DefRange: blk.DefRange(), File: path}, blk)
		case "output":
			if len(blk.Labels) != 1 {
				return fmt.Errorf("%s: output needs 1 label", blk.TypeRange)
			}
			g.add(&Node{Addr: Address{Kind: KindOutput, Name: blk.Labels[0]}, DefRange: blk.DefRange(), File: path}, blk)
		case "module":
			if len(blk.Labels) != 1 {
				return fmt.Errorf("%s: module needs 1 label", blk.TypeRange)
			}
			g.add(&Node{Addr: Address{Kind: KindModule, Name: blk.Labels[0]}, DefRange: blk.DefRange(), File: path}, blk)
		case "locals":
			for name, attr := range blk.Body.Attributes {
				n := &Node{Addr: Address{Kind: KindLocal, Name: name}, DefRange: attr.SrcRange, File: path}
				g.addLocal(n, attr)
			}
		case "terraform", "provider", "moved", "import", "check":
			// not reference-graph nodes
		}
	}
	return nil
}

// add stores a node and stashes the block body so refs can be extracted after
// all nodes are known.
func (g *Graph) add(n *Node, blk *hclsyntax.Block) {
	key := n.Addr.String()
	if _, dup := g.Nodes[key]; dup {
		// Duplicate address is a config error Terraform would also reject; the
		// second definition overwrites for graph purposes.
		g.Nodes[key] = n
	} else {
		g.Nodes[key] = n
		g.order = append(g.order, key)
	}
	g.pendingBlocks = append(g.pendingBlocks, pendingBlock{n: n, body: blk.Body})
}

func (g *Graph) addLocal(n *Node, attr *hclsyntax.Attribute) {
	key := n.Addr.String()
	if _, dup := g.Nodes[key]; !dup {
		g.order = append(g.order, key)
	}
	g.Nodes[key] = n
	g.pendingExprs = append(g.pendingExprs, pendingExpr{n: n, expr: attr.Expr})
}

type pendingBlock struct {
	n    *Node
	body *hclsyntax.Body
}
type pendingExpr struct {
	n    *Node
	expr hclsyntax.Expression
}

// refCollector accumulates referenced addresses and, per producer, every
// distinct attribute path seen at a reference. dependsOn holds addresses
// referenced from a depends_on attribute (ordering-only).
type refCollector struct {
	seen      map[string]Address
	attrs     map[string][]string
	dependsOn map[string]Address
	// providerRef holds the segments of a `provider = name.alias` meta-argument,
	// e.g. ["tls","signed"]; nil when absent.
	providerRef []string
}

func newRefCollector() *refCollector {
	return &refCollector{
		seen:      map[string]Address{},
		attrs:     map[string][]string{},
		dependsOn: map[string]Address{},
	}
}

// providerKey renders the collected provider meta-argument as "name" or
// "name.alias", or "" if none was set.
func (c *refCollector) providerKey() string {
	switch len(c.providerRef) {
	case 1:
		return c.providerRef[0]
	case 2:
		return c.providerRef[0] + "." + c.providerRef[1]
	default:
		return ""
	}
}

// resolveRefs walks every stored body/expression, extracts traversals, and
// records edges to known nodes.
func (g *Graph) resolveRefs() {
	for _, pb := range g.pendingBlocks {
		c := newRefCollector()
		g.walkBody(pb.body, c)
		pb.n.Refs = flatten(c.seen)
		pb.n.RefAttrs = c.attrs
		pb.n.DependsOnOnly = orderingOnly(c)
		pb.n.ProviderKey = c.providerKey()
	}
	for _, pe := range g.pendingExprs {
		c := newRefCollector()
		g.walkExpr(pe.expr, c)
		pe.n.Refs = flatten(c.seen)
		pe.n.RefAttrs = c.attrs
		pe.n.DependsOnOnly = orderingOnly(c)
	}
}

// orderingOnly returns the depends_on producers that are not also referenced
// for value, sorted.
func orderingOnly(c *refCollector) []Address {
	var out []Address
	for k, addr := range c.dependsOn {
		if _, valueRef := c.seen[k]; valueRef {
			continue
		}
		out = append(out, addr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func flatten(seen map[string]Address) []Address {
	out := make([]Address, 0, len(seen))
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, seen[k])
	}
	return out
}
