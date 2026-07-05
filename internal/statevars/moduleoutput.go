package statevars

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

// moduleOutputTarget resolves a module call's output to the child resource and
// attribute it exposes, so a value can be read from the module-scoped state.
//
// It reads the module block's local `source`, parses the child root's
// `output "<outputName>"` block, and extracts the leading `<type>.<name>.<attr>`
// traversal from its value expression. Returns (childResourceAddr, childAttr).
// e.g. child `output "id" { value = random_pet.id.id }` yields
// ("random_pet.id", "id").
//
// Only the common case — an output whose value is a single resource traversal —
// is resolved from state; anything more complex (a function of several values)
// can't be read from state and is reported as unresolved so the caller errors
// loudly rather than emitting a wrong value.
func moduleOutputTarget(sourceDir, moduleName, outputName string) (childRes, childAttr string, ok bool) {
	if sourceDir == "" {
		return "", "", false
	}
	source, found := moduleSource(sourceDir, moduleName)
	if !found || !isLocalSource(source) {
		return "", "", false
	}
	childDir := filepath.Join(sourceDir, source)

	expr, found := outputValueExpr(childDir, outputName)
	if !found {
		return "", "", false
	}
	segs, found := leadingTraversal(expr)
	if !found || len(segs) < 2 {
		return "", "", false
	}
	// Child resource is <type>.<name>; anything after is the attribute path.
	// (Data sources / module chains inside a child are out of scope for v1.)
	if segs[0] == "data" || segs[0] == "module" || segs[0] == "var" || segs[0] == "local" {
		return "", "", false
	}
	childRes = segs[0] + "." + segs[1]
	childAttr = strings.Join(segs[2:], ".")
	return childRes, childAttr, true
}

// moduleSource reads the `source` string of module "<name>" in the root at dir.
func moduleSource(dir, name string) (string, bool) {
	for _, path := range tfFilesIn(dir) {
		src, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		f, diags := hclwrite.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			continue
		}
		for _, blk := range f.Body().Blocks() {
			if blk.Type() != "module" || len(blk.Labels()) != 1 || blk.Labels()[0] != name {
				continue
			}
			attr := blk.Body().GetAttribute("source")
			if attr == nil {
				return "", false
			}
			s := strings.TrimSpace(string(attr.Expr().BuildTokens(nil).Bytes()))
			if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
				return s[1 : len(s)-1], true
			}
		}
	}
	return "", false
}

// outputValueExpr returns the value expression of output "<name>" in the root
// at dir.
func outputValueExpr(dir, name string) (hclsyntax.Expression, bool) {
	for _, path := range tfFilesIn(dir) {
		src, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		f, diags := hclsyntax.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			continue
		}
		body := f.Body.(*hclsyntax.Body)
		for _, blk := range body.Blocks {
			if blk.Type != "output" || len(blk.Labels) != 1 || blk.Labels[0] != name {
				continue
			}
			if attr, ok := blk.Body.Attributes["value"]; ok {
				return attr.Expr, true
			}
		}
	}
	return nil, false
}

// leadingTraversal extracts the segment names of the first scope traversal in
// an expression (e.g. random_pet.id.id -> ["random_pet","id","id"]).
func leadingTraversal(expr hclsyntax.Expression) ([]string, bool) {
	t, ok := expr.(*hclsyntax.ScopeTraversalExpr)
	if !ok {
		return nil, false
	}
	var segs []string
	for _, step := range t.Traversal {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			segs = append(segs, s.Name)
		case hcl.TraverseAttr:
			segs = append(segs, s.Name)
		default:
			return segs, len(segs) > 0
		}
	}
	return segs, len(segs) > 0
}

// isLocalSource reports whether a module source is a local filesystem path.
func isLocalSource(source string) bool {
	return strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../")
}

// tfFilesIn lists *.tf files directly in dir (non-recursive), sorted.
func tfFilesIn(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".tf" {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}
