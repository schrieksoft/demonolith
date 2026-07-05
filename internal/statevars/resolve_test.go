package statevars_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/schrieksoft/demonolith/internal/boundary"
	"github.com/schrieksoft/demonolith/internal/hclgraph"
	"github.com/schrieksoft/demonolith/internal/statevars"
	"github.com/schrieksoft/demonolith/internal/testsupport"
)

// writeState writes a minimal Terraform state file with the given managed
// resources into dir and returns its path.
func writeState(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "terraform.tfstate")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestGenerate_ResolvesFromState checks the pure resolution path without a
// terraform binary: a cross edge's producer attribute is read from state and
// written into the consumer's tfvars.
func TestGenerate_ResolvesFromState(t *testing.T) {
	base := testsupport.OutDir(t, "_unit", "resolves-from-state")
	statePath := writeState(t, base, `{
      "resources": [
        {"mode":"managed","type":"random_integer","name":"seed",
         "instances":[{"attributes":{"result":48,"id":"48"}}]}
      ]
    }`)
	st, err := statevars.LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	producer := hclgraph.Address{Kind: hclgraph.KindResource, Type: "random_integer", Name: "seed"}
	bound := &boundary.Result{
		Boundaries: map[string]*boundary.ModuleBoundary{
			"a": {Module: "a", Outputs: map[string]boundary.Output{
				"random_integer_seed": {Name: "random_integer_seed", Node: producer, Attr: "result"},
			}},
			"b": {Module: "b", Inputs: map[string]boundary.Input{
				"random_integer_seed": {Name: "random_integer_seed", FromModule: "a", FromOutput: "random_integer_seed"},
			}},
		},
		CrossEdges: []boundary.CrossEdge{{
			ProducerModule: "a", ConsumerModule: "b",
			Producer:   producer,
			OutputName: "random_integer_seed", InputName: "random_integer_seed",
		}},
	}

	bDir := filepath.Join(base, "b")
	if err := os.MkdirAll(bDir, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := statevars.Generate(st, bound, map[string]string{"a": filepath.Join(base, "a"), "b": bDir}, statevars.Options{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if got := res.Values["b"]["random_integer_seed"]; got != "48" {
		t.Errorf("resolved value = %q, want \"48\"", got)
	}
	tfvars, err := os.ReadFile(filepath.Join(bDir, statevars.TfvarsName))
	if err != nil {
		t.Fatalf("read tfvars: %v", err)
	}
	if want := `random_integer_seed = "48"`; !contains(string(tfvars), want) {
		t.Errorf("tfvars = %q, want it to contain %q", tfvars, want)
	}
}

// TestGenerate_MissingStateValueIsError confirms an input whose producer is
// absent from state is a hard error (the split would be unprovable).
func TestGenerate_MissingStateValueIsError(t *testing.T) {
	base := testsupport.OutDir(t, "_unit", "missing-state-value")
	statePath := writeState(t, base, `{"resources":[]}`)
	st, err := statevars.LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	producer := hclgraph.Address{Kind: hclgraph.KindResource, Type: "random_integer", Name: "seed"}
	bound := &boundary.Result{
		Boundaries: map[string]*boundary.ModuleBoundary{
			"a": {Module: "a", Outputs: map[string]boundary.Output{
				"random_integer_seed": {Name: "random_integer_seed", Node: producer, Attr: "result"},
			}},
			"b": {Module: "b"},
		},
		CrossEdges: []boundary.CrossEdge{{
			ProducerModule: "a", ConsumerModule: "b",
			Producer:   producer,
			OutputName: "random_integer_seed", InputName: "random_integer_seed",
		}},
	}
	_, err = statevars.Generate(st, bound, map[string]string{"a": filepath.Join(base, "a"), "b": filepath.Join(base, "b")}, statevars.Options{})
	if err == nil {
		t.Fatal("expected error for producer missing from state, got nil")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
