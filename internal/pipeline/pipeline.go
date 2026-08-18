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
}

// Options configures the analysis.
type Options struct {
	// Remainder is the catchall module name (default "legacy").
	Remainder string
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

	return &Analysis{Graph: g, Placement: p, Boundary: res}, nil
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
