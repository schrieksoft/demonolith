// Package emit writes the carved per-module roots. For each module it produces
// a subdirectory containing:
//   - the module's assigned resource/data blocks (moved verbatim via hclwrite,
//     preserving comments and formatting), with cross-module references
//     rewritten to var.<input>;
//   - generated variable blocks for the module's boundary inputs;
//   - generated output blocks for the module's boundary outputs;
//   - a root.tf holding the terraform{} block: required_providers propagated
//     from the root, plus the derived backend when one is configured.
//
// The emitted roots are detached: snapcd_* wiring is the bootstrap package's job.
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

// RootGitignore is the .gitignore written into every emitted root: the local
// artifacts an init, plan, or migration leaves behind. The engine lock file is
// deliberately absent - it belongs in version control.
const RootGitignore = `.terraform/
*.tfstate
*.tfstate.*
*.backup
demono.env
demono.tfplan
demono.root.tfvars
demono.graph.tfvars
crash.log
crash.*.log
`

// WriteGitignore writes RootGitignore into dir.
func WriteGitignore(dir string) error {
	return os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(RootGitignore), 0o644)
}

// Emitter carves a monolith into per-module roots.
type Emitter struct {
	SrcDir string
	OutDir string
	Graph  *hclgraph.Graph
	Place  *placement.Placement
	Bound  *boundary.Result
	// Monorepo relinks local child-module calls to their original in-repo
	// directories instead of copying them. Default false: carved roots are
	// standalone and shippable to separate repos.
	Monorepo bool
	// Backend, when set, writes the derived backend into each module's root.tf
	// (the monolith's block with per-module state locations).
	Backend *BackendBlock
	// PathBase, when set, replaces OutDir as the directory relative
	// module-source paths are computed against in monorepo mode - verify
	// emits into a scratch dir but must produce the source paths the real
	// roots carry.
	PathBase string
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

	// main.tf: moved blocks with rewritten references. The terraform{} block
	// goes to root.tf instead; variable declarations go to variables.tf.
	mainFile := hclwrite.NewEmptyFile()
	body := mainFile.Body()
	varFile := hclwrite.NewEmptyFile()

	// Structural blocks (provider / original variable / locals) the module uses,
	// duplicated in like required_providers.
	e.emitStructural(module, body, varFile.Body(), sb)

	blocks, err := e.movedBlocks(module)
	if err != nil {
		return EmittedModule{}, err
	}
	logicalDir := dir
	if e.PathBase != "" {
		logicalDir = filepath.Join(e.PathBase, module)
	}
	for _, blk := range blocks {
		e.rewriteRefs(module, blk)
		if e.Monorepo {
			if err := relinkModuleSource(blk, e.SrcDir, logicalDir); err != nil {
				return EmittedModule{}, err
			}
		}
		body.AppendBlock(blk)
		body.AppendNewline()
	}

	// variables.tf - boundary-derived inputs, after the original declarations
	// carved above. Skip external stand-ins for any variable whose original
	// declaration the module carries, to avoid a duplicate declaration.
	ownVars := e.neededVariableNames(module, sb)
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

	// root.tf - the terraform{} block, following the common root convention:
	// required_providers propagated from the source, plus the module's derived
	// backend when one is configured.
	if reqProviders != nil || e.Backend != nil {
		rootFile := hclwrite.NewEmptyFile()
		var tfb *hclwrite.Block
		if reqProviders != nil {
			tfb = cloneBlock(reqProviders)
			rootFile.Body().AppendBlock(tfb)
		} else {
			tfb = rootFile.Body().AppendNewBlock("terraform", nil)
		}
		if e.Backend != nil {
			bb, err := e.Backend.BackendHCL(module)
			if err != nil {
				return EmittedModule{}, err
			}
			tfb.Body().AppendNewline()
			tfb.Body().AppendBlock(bb)
		}
		if err := os.WriteFile(filepath.Join(dir, "root.tf"), hclwrite.Format(rootFile.Bytes()), 0o644); err != nil {
			return EmittedModule{}, err
		}
		em.Files = append(em.Files, "root.tf")
	}

	if err := WriteGitignore(dir); err != nil {
		return EmittedModule{}, err
	}
	em.Files = append(em.Files, ".gitignore")

	// README.md - how to run the carved root detached. Excluded from the
	// emit checksum: documentation, not part of the compared contract.
	readme := e.moduleReadme(b, ownVars)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o644); err != nil {
		return EmittedModule{}, err
	}
	em.Files = append(em.Files, "README.md")

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

// writeVariable emits `variable "<name>" { type = string }`. Every generated
// input is typed string, matching Snap CD's stringified passing.
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

// moduleReadme builds the carved root's README: the detached init/plan
// commands, with only the var files and env sourcing this module needs. The
// demono.* files it names are written by the migrate pipeline.
func (e *Emitter) moduleReadme(b *boundary.ModuleBoundary, ownVars map[string]bool) string {
	hasRoot := len(ownVars) > 0
	hasGraph := false
	if b != nil {
		for _, in := range b.Inputs {
			if in.External {
				hasRoot = true
			} else {
				hasGraph = true
			}
		}
	}
	var sb strings.Builder
	sb.WriteString("# How to run\n\n```\n")
	if e.Backend != nil {
		sb.WriteString("source " + EnvFileName + "\n")
	}
	sb.WriteString("tofu init\n")
	cmd := "tofu plan"
	if hasRoot {
		cmd += " --var-file demono.root.tfvars"
	}
	if hasGraph {
		cmd += " --var-file demono.graph.tfvars"
	}
	sb.WriteString(cmd + "\n```\n\nThe demono.* files are written by the `demonolith migrate` pipeline.\n")
	return sb.String()
}
