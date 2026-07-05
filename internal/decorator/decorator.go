// Package decorator reads Demonolith placement decorators from Terraform files.
//
// A decorator is a comment directly above (or below) a resource/module/data
// block, e.g.:
//
//	# @demono:move networking
//	resource "random_uuid" "vpc_id" { ... }
//
// Decorators are namespaced (@demono:), strict, and fail-loud: any comment that
// looks like a decorator but does not parse is a hard error, because comments
// are invisible to `terraform validate` and a silent typo would mis-place a
// resource. Decorators attach to the resolved block address, not a line.
package decorator

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// namespace is the required decorator prefix.
const namespace = "@demono:"

// decoratorLine matches a comment line that is intended as a decorator. The
// looksLike pattern is deliberately loose so that near-misses are caught and
// reported rather than silently ignored; the strict pattern then validates.
var (
	looksLikeRe = regexp.MustCompile(`@demono\b`)
	strictRe    = regexp.MustCompile(`^@demono:(\w+)\s+(.+?)\s*$`)
)

// Verb is a decorator action. v1 supports only "move".
type Verb string

const (
	VerbMove Verb = "move"
)

// Decorator is one parsed placement directive attached to a block address.
type Decorator struct {
	Verb    Verb
	Targets []string // module names; arity validated per block kind
	Range   hcl.Range
}

// BlockDecorators is the set of decorators attached to a single block, together
// with the block's identity.
type BlockDecorators struct {
	BlockType  string // "resource" | "data" | "module"
	Labels     []string
	Addr       string // canonical address string, matches hclgraph.Address.String()
	Decorators []Decorator
	DefRange   hcl.Range
}

// Error is a decorator parse/validation failure with source position.
type Error struct {
	Range hcl.Range
	Msg   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Range, e.Msg)
}

// Scan parses src (already-known filename) and returns the decorators attached
// to each decoratable block. It uses hclsyntax to locate blocks and their
// leading/trailing comment ranges, and the raw source lines to read comment
// text. Any malformed decorator is returned as an error.
func Scan(filename string, src []byte) ([]BlockDecorators, error) {
	f, diags := hclsyntax.ParseConfig(src, filename, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse %s: %s", filename, diags.Error())
	}
	body := f.Body.(*hclsyntax.Body)
	lines := splitLines(src)

	// Collect comment decorators keyed by line number so we can attach the
	// block immediately above/below a comment.
	commentsByLine, err := scanComments(filename, lines)
	if err != nil {
		return nil, err
	}

	var out []BlockDecorators
	used := map[int]bool{} // comment lines already attached to a block

	for _, blk := range body.Blocks {
		if !isDecoratable(blk.Type) {
			continue
		}
		bd := BlockDecorators{
			BlockType: blk.Type,
			Labels:    append([]string(nil), blk.Labels...),
			Addr:      addrString(blk),
			DefRange:  blk.DefRange(),
		}

		headerLine := blk.DefRange().Start.Line
		closeLine := blk.Body.SrcRange.End.Line

		// Above: contiguous decorator comment lines immediately preceding the
		// header (allowing only decorator comments in the run; a blank line or
		// non-decorator comment breaks contiguity).
		for ln := headerLine - 1; ln >= 1; ln-- {
			d, ok := commentsByLine[ln]
			if !ok {
				break
			}
			bd.Decorators = append(bd.Decorators, d)
			used[ln] = true
		}
		// Below: the line immediately after the block close.
		if d, ok := commentsByLine[closeLine+1]; ok {
			bd.Decorators = append(bd.Decorators, d)
			used[closeLine+1] = true
		}

		if err := validateArity(blk.Type, bd, blk.DefRange()); err != nil {
			return nil, err
		}
		out = append(out, bd)
	}

	// Any decorator comment not attached to a block is an error: it looked like
	// a decorator but has no block to bind to.
	var orphanLines []int
	for ln, d := range commentsByLine {
		if !used[ln] {
			orphanLines = append(orphanLines, ln)
			_ = d
		}
	}
	if len(orphanLines) > 0 {
		sort.Ints(orphanLines)
		ln := orphanLines[0]
		return nil, &Error{
			Range: commentsByLine[ln].Range,
			Msg:   fmt.Sprintf("decorator on line %d is not attached to a resource/data/module block", ln),
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out, nil
}

// scanComments walks every source line, and for each comment that looks like a
// decorator, strictly parses it. A near-miss (contains @demono but doesn't match
// the strict grammar) is a hard error.
func scanComments(filename string, lines []string) (map[int]Decorator, error) {
	out := map[int]Decorator{}
	for i, raw := range lines {
		lineNo := i + 1
		text, isComment := commentText(raw)
		if !isComment {
			continue
		}
		if !looksLikeRe.MatchString(text) {
			continue
		}
		rng := hcl.Range{
			Filename: filename,
			Start:    hcl.Pos{Line: lineNo, Column: 1},
			End:      hcl.Pos{Line: lineNo, Column: len(raw) + 1},
		}
		m := strictRe.FindStringSubmatch(strings.TrimSpace(text))
		if m == nil {
			return nil, &Error{Range: rng, Msg: fmt.Sprintf("malformed decorator %q: expected `@demono:<verb> <target>`", strings.TrimSpace(text))}
		}
		verb := m[1]
		if Verb(verb) != VerbMove {
			return nil, &Error{Range: rng, Msg: fmt.Sprintf("unknown decorator verb %q (only `move` is supported)", verb)}
		}
		targets := strings.Fields(m[2])
		if len(targets) == 0 {
			return nil, &Error{Range: rng, Msg: "decorator `move` requires at least one target module"}
		}
		if _, dup := out[lineNo]; dup {
			return nil, &Error{Range: rng, Msg: "multiple decorators on one line"}
		}
		out[lineNo] = Decorator{Verb: VerbMove, Targets: targets, Range: rng}
	}
	return out, nil
}

// validateArity enforces target arity: resource/module take exactly one target
// (stateful singleton); data may take one or more (duplicated into each).
func validateArity(blockType string, bd BlockDecorators, rng hcl.Range) error {
	total := 0
	for _, d := range bd.Decorators {
		total += len(d.Targets)
	}
	if total == 0 {
		return nil // no decorator -> catchall, handled downstream
	}
	if blockType == "resource" || blockType == "module" {
		if len(bd.Decorators) > 1 || total > 1 {
			return &Error{Range: rng, Msg: fmt.Sprintf("%s %v has %d move targets; managed/module blocks take exactly one (a stateful singleton cannot live in two roots)", blockType, bd.Labels, total)}
		}
	}
	return nil
}

func isDecoratable(t string) bool {
	return t == "resource" || t == "data" || t == "module"
}

func addrString(blk *hclsyntax.Block) string {
	switch blk.Type {
	case "resource":
		return blk.Labels[0] + "." + blk.Labels[1]
	case "data":
		return "data." + blk.Labels[0] + "." + blk.Labels[1]
	case "module":
		return "module." + blk.Labels[0]
	}
	return strings.Join(append([]string{blk.Type}, blk.Labels...), ".")
}

// commentText returns the text of a single-line comment (# or //) with the
// marker stripped, and whether the line is such a comment.
func commentText(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(trimmed, "#"):
		return strings.TrimSpace(strings.TrimPrefix(trimmed, "#")), true
	case strings.HasPrefix(trimmed, "//"):
		return strings.TrimSpace(strings.TrimPrefix(trimmed, "//")), true
	}
	return "", false
}

func splitLines(src []byte) []string {
	return strings.Split(string(src), "\n")
}
