package emit

import (
	"os"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/schrieksoft/demonolith/internal/hclgraph"
)

// tokensForIdent returns tokens rendering a bare identifier (e.g. the type
// keyword `string`), unquoted.
func tokensForIdent(name string) hclwrite.Tokens {
	return hclwrite.Tokens{
		{Type: hclsyntax.TokenIdent, Bytes: []byte(name)},
	}
}

// tokensForTraversal renders a reference like `random_uuid.vpc_id.result`:
// the node address followed by an optional attribute path.
func tokensForTraversal(node hclgraph.Address, attr string) hclwrite.Tokens {
	var toks hclwrite.Tokens
	appendName := func(s string) {
		if len(toks) > 0 {
			toks = append(toks, &hclwrite.Token{Type: hclsyntax.TokenDot, Bytes: []byte(".")})
		}
		toks = append(toks, &hclwrite.Token{Type: hclsyntax.TokenIdent, Bytes: []byte(s)})
	}
	switch node.Kind {
	case hclgraph.KindResource:
		appendName(node.Type)
		appendName(node.Name)
	case hclgraph.KindData:
		appendName("data")
		appendName(node.Type)
		appendName(node.Name)
	case hclgraph.KindModule:
		appendName("module")
		appendName(node.Name)
	default:
		appendName(node.Name)
	}
	if attr != "" {
		appendName(attr)
	}
	return toks
}

// tokensForVarRef renders `var.<name>`.
func tokensForVarRef(name string) hclwrite.Tokens {
	return hclwrite.Tokens{
		{Type: hclsyntax.TokenIdent, Bytes: []byte("var")},
		{Type: hclsyntax.TokenDot, Bytes: []byte(".")},
		{Type: hclsyntax.TokenIdent, Bytes: []byte(name)},
	}
}

// collectRequiredProviders reads the source root's terraform{} required_providers
// block (if any) and returns a clone to embed in each carved root. Returns nil
// if the root declares no providers.
func (e *Emitter) collectRequiredProviders() (*hclwrite.Block, error) {
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
			if blk.Type() != "terraform" {
				continue
			}
			if rp := blk.Body().FirstMatchingBlock("required_providers", nil); rp != nil {
				// Wrap required_providers in a fresh terraform{} block.
				tf := hclwrite.NewEmptyFile()
				tfBlk := tf.Body().AppendNewBlock("terraform", nil)
				tfBlk.Body().AppendBlock(cloneBlock(rp))
				return cloneBlock(tfBlk), nil
			}
		}
	}
	return nil, nil
}
