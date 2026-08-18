package boundary

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/schrieksoft/demonolith/internal/decorator"
	"github.com/schrieksoft/demonolith/internal/hclgraph"
	"github.com/schrieksoft/demonolith/internal/placement"
)

// buildFixture runs the phase 1+2 pipeline on the testdata monolith.
func buildFixture(t *testing.T) (*hclgraph.Graph, *placement.Placement, *Result) {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata", "monolith", "in")

	g, err := hclgraph.ParseDir(dir)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}

	var decos []decorator.BlockDecorators
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".tf" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		src, _ := os.ReadFile(path)
		bds, err := decorator.Scan(path, src)
		if err != nil {
			t.Fatalf("Scan %s: %v", e.Name(), err)
		}
		decos = append(decos, bds...)
	}

	p, err := placement.Resolve(g, decos, placement.Options{Remainder: "legacy"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	res, err := Compute(g, p)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return g, p, res
}

func TestPlacement_Assignment(t *testing.T) {
	_, p, _ := buildFixture(t)

	if got, _ := p.ModuleOf(hclgraph.Address{Kind: hclgraph.KindResource, Type: "random_uuid", Name: "vpc_id"}); got != "networking" {
		t.Errorf("vpc_id -> %q, want networking", got)
	}
	if got, _ := p.ModuleOf(hclgraph.Address{Kind: hclgraph.KindResource, Type: "random_uuid", Name: "database_id"}); got != "data" {
		t.Errorf("database_id -> %q, want data", got)
	}
	// undecorated -> catchall
	if got, _ := p.ModuleOf(hclgraph.Address{Kind: hclgraph.KindResource, Type: "random_pet", Name: "environment"}); got != "legacy" {
		t.Errorf("environment -> %q, want monolith", got)
	}
	// multi-target data source -> duplicated
	dup := p.Duplicated["data.random_id.shared_token"]
	if len(dup) != 2 {
		t.Errorf("shared_token duplicated into %v, want 2 modules", dup)
	}
}

func TestBoundary_MultiAttrProducer(t *testing.T) {
	_, _, res := buildFixture(t)

	// database_id references gateway_name through two attributes; each must
	// get its own attr-scoped input/output so each carries its own value.
	data := res.Boundaries["data"]
	net := res.Boundaries["networking"]
	for attr, name := range map[string]string{
		"id":     "random_pet_gateway_name_id",
		"prefix": "random_pet_gateway_name_prefix",
	} {
		if _, ok := data.Inputs[name]; !ok {
			t.Errorf("data missing per-attr input %q", name)
		}
		out, ok := net.Outputs[name]
		if !ok {
			t.Errorf("networking missing per-attr output %q", name)
			continue
		}
		if out.Attr != attr {
			t.Errorf("output %q exposes attr %q, want %q", name, out.Attr, attr)
		}
	}
	if _, ok := net.Outputs["random_pet_gateway_name"]; ok {
		t.Error("multi-attr producer must not also expose a plain, ambiguous output")
	}
}

func TestBoundary_CrossEdge(t *testing.T) {
	_, _, res := buildFixture(t)

	// database_id (data module) references private_subnet_id (networking) ->
	// exactly one data<-networking cross edge.
	var found *CrossEdge
	for i := range res.CrossEdges {
		e := &res.CrossEdges[i]
		if e.Producer.Name == "private_subnet_id" && e.Consumer.Name == "database_id" {
			found = e
		}
	}
	if found == nil {
		t.Fatalf("expected cross edge database_id -> private_subnet_id, edges: %+v", res.CrossEdges)
	}
	if found.ProducerModule != "networking" || found.ConsumerModule != "data" {
		t.Errorf("cross edge modules = %s->%s, want networking->data", found.ProducerModule, found.ConsumerModule)
	}

	// networking must expose the output; data must declare the input.
	net := res.Boundaries["networking"]
	if _, ok := net.Outputs[found.OutputName]; !ok {
		t.Errorf("networking missing output %q", found.OutputName)
	}
	data := res.Boundaries["data"]
	if in, ok := data.Inputs[found.InputName]; !ok || in.FromModule != "networking" {
		t.Errorf("data missing upstream input %q from networking", found.InputName)
	}
}
