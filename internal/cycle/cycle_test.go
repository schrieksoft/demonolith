package cycle_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/schrieksoft/demonolith/internal/boundary"
	"github.com/schrieksoft/demonolith/internal/cycle"
	"github.com/schrieksoft/demonolith/internal/pipeline"
)

func TestCycle_AcyclicFixtureIsClean(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "monolith", "in")
	a, err := pipeline.Analyze(dir, pipeline.Options{Remainder: "monolith"})
	if err != nil {
		t.Fatalf("Analyze acyclic fixture: %v", err)
	}
	if c := cycle.Check(a.Boundary); c != nil {
		t.Fatalf("expected no cycle, got: %s", c.Error())
	}
}

func TestCycle_CyclicFixtureIsRefused(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "cyclic", "in")
	_, err := pipeline.Analyze(dir, pipeline.Options{Remainder: "monolith"})
	if err == nil {
		t.Fatal("expected Analyze to refuse the cyclic split")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should mention cycle, got: %v", err)
	}
	// Named path should include both modules.
	if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "beta") {
		t.Errorf("cycle path should name alpha and beta, got: %v", err)
	}
}

// TestCycle_DirectResult builds a minimal boundary result by hand to confirm the
// detector's path/edge reconstruction independent of the pipeline.
func TestCycle_DirectResult(t *testing.T) {
	res := &boundary.Result{
		Boundaries: map[string]*boundary.ModuleBoundary{
			"a": {Module: "a"}, "b": {Module: "b"},
		},
		CrossEdges: []boundary.CrossEdge{
			{ConsumerModule: "a", ProducerModule: "b"},
			{ConsumerModule: "b", ProducerModule: "a"},
		},
	}
	c := cycle.Check(res)
	if c == nil {
		t.Fatal("expected a cycle")
	}
	if len(c.Path) < 2 || c.Path[0] != c.Path[len(c.Path)-1] {
		t.Errorf("path should be closed, got %v", c.Path)
	}
}
