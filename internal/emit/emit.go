// Package emit writes the carved per-module roots. For each module it produces
// a subdirectory containing:
//   - the module's assigned resource/data blocks (moved verbatim via hclwrite,
//     preserving comments and formatting), with cross-module references
//     rewritten to var.<input>;
//   - generated variable blocks for the module's boundary inputs;
//   - generated output blocks for the module's boundary outputs;
//   - a terraform{} block carrying required_providers propagated from the root.
//
// v1 emits detached roots: no snapcd_* control-plane wiring is generated.
package emit

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/schrieksoft/demonolith/internal/boundary"
	"github.com/schrieksoft/demonolith/internal/hclgraph"
	"github.com/schrieksoft/demonolith/internal/placement"
)

// Emitter carves a monolith into per-module roots.
type Emitter struct {
	SrcDir string
	OutDir string
	Graph  *hclgraph.Graph
	Place  *placement.Placement
	Bound  *boundary.Result
	// Monorepo keeps local child-module calls pointing at their original
	// in-repo directories (source rewritten to the new relative path) instead
	// of copying the directories into each carved root. Default false: carved
	// roots are fully standalone and shippable to separate repos.
	Monorepo bool
	// Backend, when set, writes each module a backend.tf derived from the
	// monolith's backend block with per-module state locations.
	Backend *BackendBlock
}

// EmittedModule records what was written for one module.
type EmittedModule struct {
	Module string
	Dir    string
	Files  []string
}

// Emit writes every module's root and returns a summary.
func (e *Emitter) Emit() ([]EmittedModule, error) {
	reqProviders, err := e.collectRequiredProviders()
	if err != nil {
		return nil, err
	}
	sb, err := e.loadSourceBlocks()
	if err != nil {
		return nil, err
	}

	var out []EmittedModule
	for _, module := range e.Place.ModuleNames() {
		em, err := e.emitModule(module, reqProviders, sb)
		if err != nil {
			return nil, fmt.Errorf("emit module %s: %w", module, err)
		}
		out = append(out, em)
	}
	return out, nil
}

// emitModule writes a single module root.
func (e *Emitter) emitModule(module string, reqProviders *hclwrite.Block, sb *sourceBlocks) (EmittedModule, error) {
	dir := filepath.Join(e.OutDir, module)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return EmittedModule{}, err
	}

	b := e.Bound.Boundaries[module]

	// main.tf: moved blocks with rewritten references.
	mainFile := hclwrite.NewEmptyFile()
	body := mainFile.Body()
	if reqProviders != nil {
		body.AppendBlock(reqProviders)
		body.AppendNewline()
	}

	// Structural blocks (provider / original variable / locals) the module uses,
	// duplicated in like required_providers.
	e.emitStructural(module, body, sb)

	blocks, err := e.movedBlocks(module)
	if err != nil {
		return EmittedModule{}, err
	}
	for _, blk := range blocks {
		e.rewriteRefs(module, blk)
		if e.Monorepo {
			if err := relinkModuleSource(blk, e.SrcDir, dir); err != nil {
				return EmittedModule{}, err
			}
		}
		body.AppendBlock(blk)
		body.AppendNewline()
	}

	// variables.tf — boundary-derived inputs. Skip external stand-ins for any
	// variable whose original declaration was carved into main.tf above, to
	// avoid a duplicate declaration.
	ownVars := e.neededVariableNames(module, sb)
	varFile := hclwrite.NewEmptyFile()
	for _, in := range sortedInputs(b) {
		if in.External && ownVars[in.SourceVar] {
			continue
		}
		writeVariable(varFile.Body(), in)
	}

	// outputs.tf
	outFile := hclwrite.NewEmptyFile()
	for _, o := range sortedOutputs(b) {
		writeOutput(outFile.Body(), o)
	}

	files := map[string][]byte{
		"main.tf":      mainFile.Bytes(),
		"variables.tf": varFile.Bytes(),
		"outputs.tf":   outFile.Bytes(),
	}

	em := EmittedModule{Module: module, Dir: dir}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		content := files[name]
		// Skip empty variables/outputs files to keep roots clean.
		if (name == "variables.tf" || name == "outputs.tf") && len(content) == 0 {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, hclwrite.Format(content), 0o644); err != nil {
			return EmittedModule{}, err
		}
		em.Files = append(em.Files, name)
	}

	if e.Backend != nil {
		if err := e.Backend.WriteBackendTF(dir, module); err != nil {
			return EmittedModule{}, err
		}
		em.Files = append(em.Files, "backend.tf")
	}

	// Copy any local child-module source directories this module owns, so the
	// carved root can resolve `source = "./..."`. In monorepo mode nothing is
	// copied: the emitted blocks were relinked to the original dirs instead.
	if !e.Monorepo {
		if err := e.copyModuleSources(module, dir); err != nil {
			return EmittedModule{}, err
		}
	}
	return em, nil
}

// relinkModuleSource rewrites a module call's local source path so it resolves
// from the carved root back to the original in-repo directory. Remote sources
// are untouched.
func relinkModuleSource(blk *hclwrite.Block, srcDir, destDir string) error {
	if blk.Type() != "module" {
		return nil
	}
	source, ok := stringAttr(blk, "source")
	if !ok || !isLocalSource(source) {
		return nil
	}
	rel, err := filepath.Rel(destDir, filepath.Join(srcDir, source))
	if err != nil {
		return fmt.Errorf("relink module source %q: %w", source, err)
	}
	rel = filepath.ToSlash(rel)
	if !strings.HasPrefix(rel, ".") {
		rel = "./" + rel
	}
	blk.Body().SetAttributeValue("source", cty.StringVal(rel))
	return nil
}

func sortedInputs(b *boundary.ModuleBoundary) []boundary.Input {
	out := make([]boundary.Input, 0, len(b.Inputs))
	for _, in := range b.Inputs {
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sortedOutputs(b *boundary.ModuleBoundary) []boundary.Output {
	out := make([]boundary.Output, 0, len(b.Outputs))
	for _, o := range b.Outputs {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// writeVariable emits `variable "<name>" { type = string }`. v1 types every
// generated input as string, matching Snap CD's stringified passing semantics;
// coercion refinements are a later concern.
func writeVariable(body *hclwrite.Body, in boundary.Input) {
	blk := body.AppendNewBlock("variable", []string{in.Name})
	// type must be the bare keyword `string`, not a quoted string.
	blk.Body().SetAttributeRaw("type", tokensForIdent("string"))
	if in.External {
		blk.Body().SetAttributeValue("description", cty.StringVal(fmt.Sprintf("External input (was var.%s in the monolith)", in.SourceVar)))
	} else {
		blk.Body().SetAttributeValue("description", cty.StringVal(fmt.Sprintf("Upstream input from module %q output %q", in.FromModule, in.FromOutput)))
	}
	body.AppendNewline()
}

// writeOutput emits `output "<name>" { value = <addr>.<attr> }`.
func writeOutput(body *hclwrite.Body, o boundary.Output) {
	blk := body.AppendNewBlock("output", []string{o.Name})
	blk.Body().SetAttributeRaw("value", tokensForTraversal(o.Node, o.Attr))
	body.AppendNewline()
}
