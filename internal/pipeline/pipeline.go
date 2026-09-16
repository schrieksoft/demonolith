// Package pipeline wires the analysis phases together: parse -> decorators ->
// placement -> boundary -> cycle gate. It is the shared front half used by both
// the CLI and tests, stopping before emission and state work.
package pipeline

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/schrieksoft/demonolith/internal/boundary"
	"github.com/schrieksoft/demonolith/internal/cycle"
	"github.com/schrieksoft/demonolith/internal/decorator"
	"github.com/schrieksoft/demonolith/internal/hclgraph"
	"github.com/schrieksoft/demonolith/internal/placement"
)

// Analysis is the result of the front-half pipeline for a monolith root.
type Analysis struct {
	Graph     *hclgraph.Graph
	Placement *placement.Placement
	Boundary  *boundary.Result
	// LegacyMove reports whether any block still uses the deprecated
	// `@demono:move` decorator, so the CLI can print the notice.
	LegacyMove bool
}

// Op selects which operation's decorators the analysis accepts.
type Op int

const (
	OpSplit Op = iota
	OpTransfer
)

// Options configures the analysis.
type Options struct {
	// Remainder is the catchall module name (default "legacy").
	Remainder string
	// Op is the operation being analyzed; each accepts its own decorator verb
	// (plus the deprecated `move`) and rejects the other's.
	Op Op
	// TransferTarget is the receiver bare `@demono:transfer` decorators place
	// into (OpTransfer only); `move` targets must agree with it when set.
	TransferTarget string
}

// Analyze runs parse -> decorators -> placement -> boundary -> cycle gate on the
// root at dir. A detected module cycle is returned as an error.
func Analyze(dir string, opts Options) (*Analysis, error) {
	g, err := hclgraph.ParseDir(dir)
	if err != nil {
		return nil, err
	}

	decos, err := scanDecorators(dir)
	if err != nil {
		return nil, err
	}
	legacy, err := applyOp(decos, opts)
	if err != nil {
		return nil, err
	}

	p, err := placement.Resolve(g, decos, placement.Options{Remainder: opts.Remainder})
	if err != nil {
		return nil, err
	}

	res, err := boundary.Compute(g, p)
	if err != nil {
		return nil, err
	}

	if c := cycle.Check(res); c != nil {
		return nil, fmt.Errorf("split refused:\n%s", c.Error())
	}

	return &Analysis{Graph: g, Placement: p, Boundary: res, LegacyMove: legacy}, nil
}

// applyOp checks every decorator's verb against the operation and resolves
// bare `transfer` decorators onto the given receiver.
func applyOp(decos []decorator.BlockDecorators, opts Options) (legacy bool, err error) {
	for i := range decos {
		bd := &decos[i]
		for j := range bd.Decorators {
			d := &bd.Decorators[j]
			switch d.Verb {
			case decorator.VerbMove:
				legacy = true
				if opts.Op == OpTransfer && opts.TransferTarget != "" {
					for _, t := range d.Targets {
						if t != opts.TransferTarget {
							return false, fmt.Errorf("%s: %s is decorated for receiver %q but --transfer-target is %q; a transfer has one receiver - run one transfer per receiver", d.Range, bd.Addr, t, opts.TransferTarget)
						}
					}
				}
			case decorator.VerbSplit:
				if opts.Op == OpTransfer {
					return false, fmt.Errorf("%s: %s carries a `@demono:split` decorator; a transfer marks its blocks with a bare `# @demono:transfer` and names the receiver as --transfer-target", d.Range, bd.Addr)
				}
			case decorator.VerbTransfer:
				if opts.Op == OpSplit {
					return false, fmt.Errorf("%s: %s carries a `@demono:transfer` decorator; a split places blocks with `# @demono:split <module>`", d.Range, bd.Addr)
				}
				if opts.TransferTarget == "" {
					return false, fmt.Errorf("%s: %s carries a bare `@demono:transfer` decorator; pass the receiver as `transfer refactor --transfer-target`", d.Range, bd.Addr)
				}
				d.Targets = []string{opts.TransferTarget}
			}
		}
	}
	return legacy, nil
}

// scanDecorators reads every *.tf file in dir and collects decorators.
func scanDecorators(dir string) ([]decorator.BlockDecorators, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []decorator.BlockDecorators
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".tf" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		bds, err := decorator.Scan(path, src)
		if err != nil {
			return nil, err
		}
		out = append(out, bds...)
	}
	return out, nil
}
