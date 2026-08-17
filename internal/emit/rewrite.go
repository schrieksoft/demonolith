package emit

import (
	"strings"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/schrieksoft/demonolith/internal/hclgraph"
)

// crossRefMap builds, for a given consumer module, the mapping from a producer
// address string to the input variable name per referenced attribute path. A
// producer referenced through several attributes has one input per attribute.
func (e *Emitter) crossRefMap(module string) map[string]map[string]string {
	m := map[string]map[string]string{}
	for _, edge := range e.Bound.CrossEdges {
		if edge.ConsumerModule != module {
			continue
		}
		key := edge.Producer.String()
		if m[key] == nil {
			m[key] = map[string]string{}
		}
		m[key][edge.Attr] = edge.InputName
	}
	return m
}

// rewriteRefs rewrites, in place, every cross-module reference inside a block's
// attributes to `var.<input>`. It walks each attribute's token stream, detects
// traversals whose leading segments name a cross-module producer, and replaces
// those tokens (including any trailing attribute access like `.result`, and any
// index like `[0]`) with a var reference.
//
// depends_on entries that point at a producer outside this module are dropped —
// whether the producer carries a value edge (the dependency is now expressed
// through the input variable) or an ordering-only edge (carried by an
// OrderingEdge). Either way, referencing a block that no longer exists in this
// root would break the root.
func (e *Emitter) rewriteRefs(module string, blk *hclwrite.Block) {
	xref := e.crossRefMap(module)
	e.rewriteBody(blk.Body(), xref, e.foreignProducer(module))
}

// foreignProducer reports whether an address names a placed block that does not
// live in the given module. Unknown addresses are left alone.
func (e *Emitter) foreignProducer(module string) func(hclgraph.Address) bool {
	return func(addr hclgraph.Address) bool {
		mods := e.Place.ModulesOf(addr)
		if len(mods) == 0 {
			return false
		}
		for _, m := range mods {
			if m == module {
				return false
			}
		}
		return true
	}
}

func (e *Emitter) rewriteBody(body *hclwrite.Body, xref map[string]map[string]string, foreign func(hclgraph.Address) bool) {
	for name, attr := range body.Attributes() {
		if name == "depends_on" {
			e.rewriteDependsOn(body, attr, foreign)
			continue
		}
		if len(xref) == 0 {
			continue
		}
		toks := attr.Expr().BuildTokens(nil)
		newToks, changed := rewriteTokens(toks, xref)
		if changed {
			body.SetAttributeRaw(name, newToks)
		}
	}
	for _, nested := range body.Blocks() {
		e.rewriteBody(nested.Body(), xref, foreign)
	}
}

// rewriteDependsOn removes out-of-module producers from a depends_on list. If
// the list becomes empty, the attribute is removed entirely.
func (e *Emitter) rewriteDependsOn(body *hclwrite.Body, attr *hclwrite.Attribute, foreign func(hclgraph.Address) bool) {
	toks := attr.Expr().BuildTokens(nil)
	kept, anyKept := filterDependsOn(toks, foreign)
	if !anyKept {
		body.RemoveAttribute("depends_on")
		return
	}
	body.SetAttributeRaw("depends_on", kept)
}

// rewriteTokens scans a token slice for traversals matching a cross-module
// producer and replaces each with `var.<input>`. It returns the new tokens and
// whether anything changed.
//
// A traversal is a run of TokenIdent separated by TokenDot. We greedily match
// the longest known producer prefix (2 segs for resource, 3 for data), then
// consume any trailing `.attr` and `[...]` steps that belong to the same
// reference, replacing the whole run.
func rewriteTokens(toks hclwrite.Tokens, xref map[string]map[string]string) (hclwrite.Tokens, bool) {
	var out hclwrite.Tokens
	changed := false
	i := 0
	for i < len(toks) {
		if match, inputName, consumed := matchProducer(toks, i, xref); match {
			out = append(out, tokensForVarRef(inputName)...)
			i += consumed
			changed = true
			continue
		}
		out = append(out, toks[i])
		i++
	}
	return out, changed
}

// matchProducer tries to match a cross-module producer traversal starting at
// index i. On success it returns the input variable name and how many tokens
// the whole reference (including trailing .attr and [idx] steps) spans. The
// referenced attribute path (the ident segments after the producer prefix,
// mirroring what the graph recorded) selects among per-attribute inputs.
func matchProducer(toks hclwrite.Tokens, i int, xref map[string]map[string]string) (bool, string, int) {
	// A traversal must start at an identifier that is not preceded by a dot
	// (which would make it an attribute of something else).
	if i > 0 && toks[i-1].Type == hclsyntax.TokenDot {
		return false, "", 0
	}
	segs, segEnd := readIdentPath(toks, i)
	if len(segs) == 0 {
		return false, "", 0
	}
	// Try the longest matching producer prefix: data.<t>.<n> (3) or <t>.<n> (2).
	for _, n := range []int{3, 2} {
		if len(segs) < n {
			continue
		}
		addr, ok := hclgraph.ParseRefRoot(segs[:n])
		if !ok {
			continue
		}
		byAttr, isCross := xref[addr.String()]
		if !isCross {
			continue
		}
		attr := strings.Join(segs[n:], ".")
		input, ok := byAttr[attr]
		if !ok {
			// A same-module reference through an attribute nobody wires (or a
			// whole-object edge) falls back to the sole input when unambiguous.
			if len(byAttr) == 1 {
				for _, v := range byAttr {
					input = v
				}
			} else if v, has := byAttr[""]; has {
				input = v
			} else {
				continue
			}
		}
		// Consume the whole reference: the n matched segments plus any
		// remaining trailing .attr / [idx] steps up to segEnd, and any
		// following index tokens.
		end := consumeTrailing(toks, segEnd)
		return true, input, end - i
	}
	return false, "", 0
}

// readIdentPath reads a dotted identifier path (ident (. ident)*) starting at i,
// returning the segment strings and the index just past the last consumed token.
func readIdentPath(toks hclwrite.Tokens, i int) ([]string, int) {
	var segs []string
	j := i
	for j < len(toks) {
		if toks[j].Type != hclsyntax.TokenIdent {
			break
		}
		segs = append(segs, string(toks[j].Bytes))
		j++
		if j < len(toks) && toks[j].Type == hclsyntax.TokenDot {
			j++
			continue
		}
		break
	}
	return segs, j
}

// consumeTrailing advances past any remaining `.ident` and `[...]` steps that
// are part of the same reference expression, so the entire traversal is
// replaced by the var reference.
func consumeTrailing(toks hclwrite.Tokens, j int) int {
	for j < len(toks) {
		switch toks[j].Type {
		case hclsyntax.TokenDot:
			if j+1 < len(toks) && toks[j+1].Type == hclsyntax.TokenIdent {
				j += 2
				continue
			}
			return j
		case hclsyntax.TokenOBrack:
			depth := 0
			for j < len(toks) {
				if toks[j].Type == hclsyntax.TokenOBrack {
					depth++
				} else if toks[j].Type == hclsyntax.TokenCBrack {
					depth--
					j++
					if depth == 0 {
						break
					}
					continue
				}
				j++
			}
			continue
		default:
			return j
		}
	}
	return j
}

// filterDependsOn rebuilds a depends_on list expression, dropping entries that
// reference producers outside this module. Returns the rebuilt tokens and
// whether any entry remains.
func filterDependsOn(toks hclwrite.Tokens, foreign func(hclgraph.Address) bool) (hclwrite.Tokens, bool) {
	// Find the bracketed list body.
	start, end := -1, -1
	depth := 0
	for i, t := range toks {
		if t.Type == hclsyntax.TokenOBrack {
			if depth == 0 {
				start = i
			}
			depth++
		} else if t.Type == hclsyntax.TokenCBrack {
			depth--
			if depth == 0 {
				end = i
				break
			}
		}
	}
	if start < 0 || end < 0 {
		return toks, true // not a list we understand; leave as-is
	}

	// Split inner tokens on top-level commas into element token runs.
	inner := toks[start+1 : end]
	var elems []hclwrite.Tokens
	var cur hclwrite.Tokens
	d := 0
	for _, t := range inner {
		switch t.Type {
		case hclsyntax.TokenOBrack, hclsyntax.TokenOParen, hclsyntax.TokenOBrace:
			d++
		case hclsyntax.TokenCBrack, hclsyntax.TokenCParen, hclsyntax.TokenCBrace:
			d--
		case hclsyntax.TokenComma:
			if d == 0 {
				elems = append(elems, cur)
				cur = nil
				continue
			}
		}
		cur = append(cur, t)
	}
	if len(trimSpace(cur)) > 0 {
		elems = append(elems, cur)
	}

	var kept []hclwrite.Tokens
	for _, el := range elems {
		if dependsOnRefsCross(el, foreign) {
			continue
		}
		if len(trimSpace(el)) == 0 {
			continue
		}
		kept = append(kept, el)
	}
	if len(kept) == 0 {
		return nil, false
	}

	// Rebuild: [ el0, el1, ... ]
	out := hclwrite.Tokens{{Type: hclsyntax.TokenOBrack, Bytes: []byte("[")}}
	for k, el := range kept {
		out = append(out, trimSpace(el)...)
		if k < len(kept)-1 {
			out = append(out, &hclwrite.Token{Type: hclsyntax.TokenComma, Bytes: []byte(",")})
			out = append(out, &hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte(" ")})
		}
	}
	out = append(out, &hclwrite.Token{Type: hclsyntax.TokenCBrack, Bytes: []byte("]")})
	return out, true
}

// dependsOnRefsCross reports whether a depends_on element references a
// producer outside this module.
func dependsOnRefsCross(el hclwrite.Tokens, foreign func(hclgraph.Address) bool) bool {
	trimmed := trimSpace(el)
	segs, _ := readIdentPath(trimmed, 0)
	for _, n := range []int{3, 2} {
		if len(segs) < n {
			continue
		}
		if addr, ok := hclgraph.ParseRefRoot(segs[:n]); ok {
			if foreign(addr) {
				return true
			}
		}
	}
	return false
}

// trimSpace drops leading/trailing whitespace/newline/comment tokens.
func trimSpace(toks hclwrite.Tokens) hclwrite.Tokens {
	start, end := 0, len(toks)
	for start < end && isSpace(toks[start]) {
		start++
	}
	for end > start && isSpace(toks[end-1]) {
		end--
	}
	return toks[start:end]
}

// isSpace reports whether a token is layout-only. In hclwrite, inline spacing
// is carried as SpacesBefore on the following token, so the only standalone
// layout tokens are newlines and comments.
func isSpace(t *hclwrite.Token) bool {
	switch t.Type {
	case hclsyntax.TokenNewline, hclsyntax.TokenComment:
		return true
	}
	return false
}
