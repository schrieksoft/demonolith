package emit

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/schrieksoft/demonolith/internal/hclgraph"
)

// structural blocks (provider, locals, variable) are not placed by the
// decorator pass — they are duplicated into every module that uses them, the
// same way required_providers is propagated. This file computes, per module,
// which of those blocks are needed and emits them (with cross-module references
// inside locals rewritten to var.<input>).

// moduleNeeds captures the structural blocks a single module must carry.
type moduleNeeds struct {
	providers map[string]bool // provider local names (e.g. "random")
	locals    map[string]bool // local names referenced (transitively)
	variables map[string]bool // variable names referenced (transitively)
}

// computeNeeds walks a module's placed resource/data/module nodes and follows
// their references (transitively through locals) to determine which providers,
// locals, and variables the module needs. It then walks the body of each needed
// provider block, so a var/local referenced only from provider config is pulled
// in too.
func (e *Emitter) computeNeeds(module string, sb *sourceBlocks) moduleNeeds {
	n := moduleNeeds{
		providers: map[string]bool{},
		locals:    map[string]bool{},
		variables: map[string]bool{},
	}
	seen := map[string]bool{}

	for _, addr := range e.Place.Modules[module] {
		node := e.Graph.Node(addr)
		switch addr.Kind {
		case hclgraph.KindResource, hclgraph.KindData:
			// A module call brings its own providers (declared in the child), so
			// only managed resources/data induce a provider need here. Honor an
			// explicit `provider = name.alias` selection; else the default.
			key := providerOf(addr.Type)
			if node != nil && node.ProviderKey != "" {
				key = node.ProviderKey
			}
			n.providers[key] = true
		}
		if node == nil {
			continue
		}
		e.collectRefsInto(node.Refs, &n, seen)
	}

	// Provider config bodies may reference var/local (e.g. region, endpoint) or
	// even a cross-module resource output. Pull those in for every needed
	// provider that has a config block.
	if sb != nil {
		for name := range n.providers {
			blk, ok := sb.providers[name]
			if !ok {
				continue
			}
			e.collectRefsInto(blockRefs(blk), &n, seen)
		}
	}
	return n
}

// collectRefsInto records the var/local references in refs, recursing into any
// referenced local so a local that itself uses a variable (or another local)
// pulls in that dependency. seen guards against local cycles.
func (e *Emitter) collectRefsInto(refs []hclgraph.Address, n *moduleNeeds, seen map[string]bool) {
	for _, ref := range refs {
		switch ref.Kind {
		case hclgraph.KindVariable:
			n.variables[ref.Name] = true
		case hclgraph.KindLocal:
			if seen[ref.Name] {
				continue
			}
			seen[ref.Name] = true
			n.locals[ref.Name] = true
			if ln := e.Graph.Node(ref); ln != nil {
				e.collectRefsInto(ln.Refs, n, seen)
			}
		}
	}
}

// blockRefs re-parses an hclwrite block and returns the var/local/resource/
// module addresses referenced anywhere in its body, using the same AST-traversal
// extraction as the main graph so refs inside nested blocks (e.g. a provider's
// proxy{} block) are caught.
func blockRefs(blk *hclwrite.Block) []hclgraph.Address {
	src := blk.BuildTokens(nil).Bytes()
	f, diags := hclsyntax.ParseConfig(src, "block", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil
	}
	body, ok := f.Body.(*hclsyntax.Body)
	if !ok {
		return nil
	}
	seen := map[string]hclgraph.Address{}
	var walkBody func(b *hclsyntax.Body)
	walkBody = func(b *hclsyntax.Body) {
		for _, attr := range b.Attributes {
			// refWalker never emits diagnostics, so the return is safely ignored.
			_ = hclsyntax.Walk(attr.Expr, refWalker(func(segs []string) {
				if addr, ok := hclgraph.ParseRefRoot(segs); ok {
					seen[addr.String()] = addr
				}
			}))
		}
		for _, nested := range b.Blocks {
			walkBody(nested.Body)
		}
	}
	walkBody(body)

	out := make([]hclgraph.Address, 0, len(seen))
	for _, a := range seen {
		out = append(out, a)
	}
	return out
}

// refWalker adapts a segment-collecting func to hclsyntax.Walker, pulling the
// leading name segments from every traversal it visits.
type refWalker func([]string)

func (f refWalker) Enter(node hclsyntax.Node) hcl.Diagnostics {
	if te, ok := node.(*hclsyntax.ScopeTraversalExpr); ok {
		var segs []string
		for _, step := range te.Traversal {
			switch s := step.(type) {
			case hcl.TraverseRoot:
				segs = append(segs, s.Name)
			case hcl.TraverseAttr:
				segs = append(segs, s.Name)
			default:
				f(segs)
				return nil
			}
		}
		f(segs)
	}
	return nil
}

func (f refWalker) Exit(hclsyntax.Node) hcl.Diagnostics { return nil }

// providerOf derives the provider local name from a resource/data type: the
// segment before the first underscore (random_integer -> random). Types without
// an underscore are their own provider name.
func providerOf(resourceType string) string {
	if i := strings.IndexByte(resourceType, '_'); i > 0 {
		return resourceType[:i]
	}
	return resourceType
}

// sourceBlocks lazily parses every source file once and indexes the structural
// blocks we may need to clone.
type sourceBlocks struct {
	providers map[string]*hclwrite.Block // name -> provider block
	locals    map[string]*hclwrite.Block // local name -> the locals{} block it lives in
	localExpr map[string]hclwrite.Tokens // local name -> its value expression tokens
	variables map[string]*hclwrite.Block // variable name -> variable block
}

// loadSourceBlocks reads all source *.tf and collects provider/locals/variable
// blocks for later per-module cloning.
func (e *Emitter) loadSourceBlocks() (*sourceBlocks, error) {
	sb := &sourceBlocks{
		providers: map[string]*hclwrite.Block{},
		locals:    map[string]*hclwrite.Block{},
		localExpr: map[string]hclwrite.Tokens{},
		variables: map[string]*hclwrite.Block{},
	}
	files, err := tfFiles(e.SrcDir)
	if err != nil {
		return nil, err
	}
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		f, diags := hclwrite.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return nil, diags
		}
		for _, blk := range f.Body().Blocks() {
			switch blk.Type() {
			case "provider":
				if len(blk.Labels()) == 1 {
					// Key by "name" or "name.alias" so an aliased provider is
					// selectable by a resource's `provider = name.alias` meta-arg.
					key := blk.Labels()[0]
					if alias, ok := stringAttr(blk, "alias"); ok && alias != "" {
						key += "." + alias
					}
					sb.providers[key] = blk
				}
			case "variable":
				if len(blk.Labels()) == 1 {
					sb.variables[blk.Labels()[0]] = blk
				}
			case "locals":
				for name, attr := range blk.Body().Attributes() {
					sb.localExpr[name] = attr.Expr().BuildTokens(nil)
					sb.locals[name] = blk
				}
			}
		}
	}
	return sb, nil
}

// emitStructural appends the module's needed provider, variable, and locals
// blocks to body. Locals' cross-module value references are rewritten to
// var.<input>, mirroring how moved resource bodies are rewritten.
func (e *Emitter) emitStructural(module string, body *hclwrite.Body, sb *sourceBlocks) {
	needs := e.computeNeeds(module, sb)
	xref := e.crossRefMap(module)

	// Providers (sorted). A provider config that references a cross-module
	// producer is rewritten to var.<input>, exactly like a moved resource body.
	for _, name := range sortedKeys(needs.providers) {
		blk, ok := sb.providers[name]
		if !ok {
			continue // implicit/default provider with no config block; nothing to carve
		}
		clone := cloneBlock(blk)
		if len(xref) > 0 {
			e.rewriteBody(clone.Body(), xref, e.foreignProducer(module))
		}
		body.AppendBlock(clone)
		body.AppendNewline()
	}

	// Variables (original declarations, preserving type/default).
	for _, name := range sortedKeys(needs.variables) {
		if blk, ok := sb.variables[name]; ok {
			body.AppendBlock(cloneBlock(blk))
			body.AppendNewline()
		}
	}

	// Locals: emit one locals{} block holding every needed local, in sorted
	// order, with cross-module value refs rewritten to var.<input>.
	names := sortedKeys(needs.locals)
	var present []string
	for _, name := range names {
		if _, ok := sb.localExpr[name]; ok {
			present = append(present, name)
		}
	}
	if len(present) > 0 {
		blk := body.AppendNewBlock("locals", nil)
		for _, name := range present {
			toks := sb.localExpr[name]
			if len(xref) > 0 {
				if rewritten, changed := rewriteTokens(toks, xref); changed {
					toks = rewritten
				}
			}
			blk.Body().SetAttributeRaw(name, toks)
		}
		body.AppendNewline()
	}
}

// neededVariableNames returns the set of variable names a module carries its own
// original declaration for, so boundary-derived external stand-ins can be
// skipped to avoid a duplicate declaration.
func (e *Emitter) neededVariableNames(module string, sb *sourceBlocks) map[string]bool {
	needs := e.computeNeeds(module, sb)
	out := map[string]bool{}
	for name := range needs.variables {
		if _, ok := sb.variables[name]; ok {
			out[name] = true
		}
	}
	return out
}

// copyModuleSources copies the local source directory of every module call
// owned by `module` into the carved root at destDir, preserving the relative
// source path so `source = "./..."` still resolves. Remote sources (registry,
// git, etc.) are left untouched.
func (e *Emitter) copyModuleSources(module, destDir string) error {
	// Which module-call addresses live in this module?
	owned := map[string]bool{}
	for _, addr := range e.Place.Modules[module] {
		if addr.Kind == hclgraph.KindModule {
			owned[addr.String()] = true
		}
	}
	if len(owned) == 0 {
		return nil
	}

	files, err := tfFiles(e.SrcDir)
	if err != nil {
		return err
	}
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, diags := hclwrite.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return diags
		}
		for _, blk := range f.Body().Blocks() {
			if blk.Type() != "module" || len(blk.Labels()) != 1 {
				continue
			}
			addr := hclgraph.Address{Kind: hclgraph.KindModule, Name: blk.Labels()[0]}
			if !owned[addr.String()] {
				continue
			}
			source, ok := stringAttr(blk, "source")
			if !ok || !isLocalSource(source) {
				continue // remote source: nothing to copy
			}
			srcPath := filepath.Join(e.SrcDir, source)
			dstPath := filepath.Join(destDir, source)
			if err := copyTree(srcPath, dstPath); err != nil {
				return fmt.Errorf("copy module source %q: %w", source, err)
			}
		}
	}
	return nil
}

// stringAttr reads a bare double-quoted string attribute value from a block.
func stringAttr(blk *hclwrite.Block, name string) (string, bool) {
	attr := blk.Body().GetAttribute(name)
	if attr == nil {
		return "", false
	}
	s := strings.TrimSpace(string(attr.Expr().BuildTokens(nil).Bytes()))
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1], true
	}
	return "", false
}

// isLocalSource reports whether a module source is a local filesystem path.
func isLocalSource(source string) bool {
	return strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../")
}

// copyTree recursively copies the directory tree at src to dst.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyOneFile(src, dst)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		// Skip terraform working-dir artifacts.
		if ent.Name() == ".terraform" || ent.Name() == ".terraform.lock.hcl" {
			continue
		}
		if err := copyTree(filepath.Join(src, ent.Name()), filepath.Join(dst, ent.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyOneFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
