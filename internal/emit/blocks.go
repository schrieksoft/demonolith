package emit

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/schrieksoft/demonolith/internal/hclgraph"
)

// movedBlocks returns hclwrite blocks (clones) for every resource/data block
// assigned to module, read from the source files with formatting preserved.
// A duplicated data source is cloned into each of its target modules, so this
// returns its block when module is one of the targets.
func (e *Emitter) movedBlocks(module string) ([]*hclwrite.Block, error) {
	// Build the set of addresses that belong in this module.
	want := map[string]bool{}
	for _, addr := range e.Place.Modules[module] {
		want[addr.String()] = true
	}

	// Group source files, parse each with hclwrite, pull matching blocks.
	files, err := tfFiles(e.SrcDir)
	if err != nil {
		return nil, err
	}

	var collected []*hclwrite.Block
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
			addr, ok := blockAddr(blk)
			if !ok || !want[addr.String()] {
				continue
			}
			collected = append(collected, cloneBlockStripped(blk))
		}
	}

	// Stable order: resources/data by address.
	sort.Slice(collected, func(i, j int) bool {
		ai, _ := blockAddr(collected[i])
		aj, _ := blockAddr(collected[j])
		return ai.String() < aj.String()
	})
	return collected, nil
}

// blockAddr maps an hclwrite block to a graph Address, for resource/data/module.
func blockAddr(blk *hclwrite.Block) (hclgraph.Address, bool) {
	labels := blk.Labels()
	switch blk.Type() {
	case "resource":
		if len(labels) == 2 {
			return hclgraph.Address{Kind: hclgraph.KindResource, Type: labels[0], Name: labels[1]}, true
		}
	case "data":
		if len(labels) == 2 {
			return hclgraph.Address{Kind: hclgraph.KindData, Type: labels[0], Name: labels[1]}, true
		}
	case "module":
		if len(labels) == 1 {
			return hclgraph.Address{Kind: hclgraph.KindModule, Name: labels[0]}, true
		}
	}
	return hclgraph.Address{}, false
}

// cloneBlock deep-copies an hclwrite block by round-tripping its tokens through
// a fresh file, so the returned block is detached from its source file and safe
// to mutate and re-parent.
func cloneBlock(blk *hclwrite.Block) *hclwrite.Block {
	tmp := hclwrite.NewEmptyFile()
	tmp.Body().AppendBlock(blk)
	// AppendBlock moves the block; re-parse the serialized form to fully detach.
	reparsed, _ := hclwrite.ParseConfig(tmp.Bytes(), "clone", hcl.Pos{Line: 1, Column: 1})
	return reparsed.Body().Blocks()[0]
}

// cloneBlockStripped clones a block and removes any @demono: decorator comment
// lines, which are stale in a carved root (the block is already placed) and
// would be actively misleading on a duplicated data source.
func cloneBlockStripped(blk *hclwrite.Block) *hclwrite.Block {
	return stripDecoratorTokens(cloneBlock(blk))
}

// stripDecoratorTokens rebuilds the block from its serialized form with any
// source line containing a @demono: decorator comment removed.
func stripDecoratorTokens(blk *hclwrite.Block) *hclwrite.Block {
	tmp := hclwrite.NewEmptyFile()
	tmp.Body().AppendBlock(blk)
	src := tmp.Bytes()

	var kept [][]byte
	for _, line := range bytesSplitLines(src) {
		trimmed := trimLeadingSpace(line)
		if isDecoratorComment(trimmed) {
			continue
		}
		kept = append(kept, line)
	}
	joined := bytesJoinLines(kept)
	reparsed, _ := hclwrite.ParseConfig(joined, "stripped", hcl.Pos{Line: 1, Column: 1})
	blks := reparsed.Body().Blocks()
	if len(blks) == 0 {
		return blk
	}
	return blks[0]
}

// isDecoratorComment reports whether a trimmed line is a @demono: decorator
// comment (# or // form).
func isDecoratorComment(line []byte) bool {
	s := string(line)
	if !strings.Contains(s, "@demono:") {
		return false
	}
	return strings.HasPrefix(s, "#") || strings.HasPrefix(s, "//")
}

func bytesSplitLines(b []byte) [][]byte {
	return splitKeepNoEOL(b)
}

// splitKeepNoEOL splits on '\n' without keeping the terminators.
func splitKeepNoEOL(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	out = append(out, b[start:])
	return out
}

func bytesJoinLines(lines [][]byte) []byte {
	var out []byte
	for i, l := range lines {
		out = append(out, l...)
		if i < len(lines)-1 {
			out = append(out, '\n')
		}
	}
	return out
}

func trimLeadingSpace(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t') {
		i++
	}
	return b[i:]
}

func tfFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".tf" {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	sort.Strings(files)
	return files, nil
}
