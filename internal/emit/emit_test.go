package emit_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/pipeline"
	"github.com/schrieksoft/demonolith/internal/testsupport"
)

// carveFixture analyzes testdata/monolith/in and emits the carved roots into
// testdata/monolith/out/emit-asserts (wiped each run), returning that dir. The
// emit assertion tests read from it; because it's under out/ the carved roots
// stay inspectable after a run.
func carveFixture(t *testing.T) string {
	t.Helper()
	src := testsupport.InDir("monolith")
	outDir := testsupport.OutDir(t, "monolith", "emit-asserts")
	a, err := pipeline.Analyze(src, pipeline.Options{Remainder: "monolith"})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	e := &emit.Emitter{SrcDir: src, OutDir: outDir, Graph: a.Graph, Place: a.Placement, Bound: a.Boundary}
	if _, err := e.Emit(); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return outDir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// assertParses confirms a carved root file is syntactically valid HCL.
func assertParses(t *testing.T, path string) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		return // missing optional file is fine
	}
	_, diags := hclsyntax.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Errorf("%s does not parse: %s", path, diags.Error())
	}
}

func TestEmit_CarvedRootsParse(t *testing.T) {
	out := carveFixture(t)
	for _, mod := range []string{"networking", "data", "monolith"} {
		for _, f := range []string{"main.tf", "variables.tf", "outputs.tf"} {
			assertParses(t, filepath.Join(out, mod, f))
		}
	}
}

func TestEmit_CrossRefRewritten(t *testing.T) {
	out := carveFixture(t)

	dataMain := readFile(t, filepath.Join(out, "data", "main.tf"))
	// The cross-module ref must be rewritten to a var, not the original address.
	if strings.Contains(dataMain, "random_uuid.private_subnet_id.result") {
		t.Errorf("data/main.tf still contains raw cross-module ref:\n%s", dataMain)
	}
	if !strings.Contains(dataMain, "var.random_uuid_private_subnet_id") {
		t.Errorf("data/main.tf missing rewritten var ref:\n%s", dataMain)
	}

	// data module must declare the input variable.
	dataVars := readFile(t, filepath.Join(out, "data", "variables.tf"))
	if !strings.Contains(dataVars, `variable "random_uuid_private_subnet_id"`) {
		t.Errorf("data/variables.tf missing generated input:\n%s", dataVars)
	}

	// networking module must expose the output.
	netOut := readFile(t, filepath.Join(out, "networking", "outputs.tf"))
	if !strings.Contains(netOut, `output "random_uuid_private_subnet_id"`) {
		t.Errorf("networking/outputs.tf missing generated output:\n%s", netOut)
	}
	if !strings.Contains(netOut, "random_uuid.private_subnet_id") {
		t.Errorf("networking output should reference the resource:\n%s", netOut)
	}
}

func TestEmit_MonorepoRelinksLocalModules(t *testing.T) {
	src, err := filepath.Abs(filepath.Join("..", "..", "testdata", "e2e-split", "in"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := pipeline.Analyze(src, pipeline.Options{})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	out := testsupport.OutDir(t, "e2e-split", "emit-monorepo")
	e := &emit.Emitter{SrcDir: src, OutDir: out, Graph: a.Graph, Place: a.Placement, Bound: a.Boundary, Monorepo: true}
	if _, err := e.Emit(); err != nil {
		t.Fatalf("emit: %v", err)
	}

	// The child-module dir must NOT be copied into the carved root...
	if _, err := os.Stat(filepath.Join(out, "network", "modules", "idgen")); !os.IsNotExist(err) {
		t.Error("monorepo mode must not copy the child module into the carved root")
	}
	// ...and the module call must point back at the original dir.
	main := readFile(t, filepath.Join(out, "network", "main.tf"))
	rel, err := filepath.Rel(filepath.Join(out, "network"), filepath.Join(src, "modules", "idgen"))
	if err != nil {
		t.Fatal(err)
	}
	want := `source = "` + filepath.ToSlash(rel) + `"`
	if !strings.Contains(main, want) {
		t.Errorf("network/main.tf missing relinked %s:\n%s", want, main)
	}
	if _, err := os.Stat(filepath.Join(src, "modules", "idgen", "main.tf")); err != nil {
		t.Errorf("relinked target must exist: %v", err)
	}
}

func TestEmit_OrderingOnlyDependsOnDropped(t *testing.T) {
	out := carveFixture(t)
	// vpc_id (networking) has depends_on = [time_sleep.wait_10s], an
	// ordering-only producer that stays in the remainder. The emitted
	// networking root must not reference a block it does not contain.
	netMain := readFile(t, filepath.Join(out, "networking", "main.tf"))
	if strings.Contains(netMain, "time_sleep.wait_10s") {
		t.Errorf("networking/main.tf still references the ordering-only producer:\n%s", netMain)
	}
}

func TestEmit_DataSourceDuplicated(t *testing.T) {
	out := carveFixture(t)
	// shared_token data source duplicated into both networking and data.
	for _, mod := range []string{"networking", "data"} {
		main := readFile(t, filepath.Join(out, mod, "main.tf"))
		if !strings.Contains(main, `data "random_id" "shared_token"`) {
			t.Errorf("%s/main.tf missing duplicated data source:\n%s", mod, main)
		}
	}
}

func TestEmit_DecoratorsStripped(t *testing.T) {
	out := carveFixture(t)
	for _, mod := range []string{"networking", "data", "monolith"} {
		main := readFile(t, filepath.Join(out, mod, "main.tf"))
		if strings.Contains(main, "@demono:") {
			t.Errorf("%s/main.tf still contains decorator comments:\n%s", mod, main)
		}
	}
}

func TestEmit_OutputExposesReferencedAttr(t *testing.T) {
	out := carveFixture(t)
	netOut := readFile(t, filepath.Join(out, "networking", "outputs.tf"))
	// The consumer used .result, so the output must expose .result — not the
	// whole resource object (which would type-mismatch the string input).
	if !strings.Contains(netOut, "random_uuid.private_subnet_id.result") {
		t.Errorf("networking output should expose .result:\n%s", netOut)
	}
}

func TestEmit_ProviderPropagated(t *testing.T) {
	out := carveFixture(t)
	net := readFile(t, filepath.Join(out, "networking", "main.tf"))
	if !strings.Contains(net, "required_providers") || !strings.Contains(net, "hashicorp/random") {
		t.Errorf("networking/main.tf missing propagated providers:\n%s", net)
	}
}

// TestEmit_ProviderAliasAndCrossModuleRef carves the e2e-split fixture offline
// (no apply/state needed) and checks the aliased provider and the provider's
// cross-module reference handling in isolation from the heavy e2e proof.
func TestEmit_ProviderAliasAndCrossModuleRef(t *testing.T) {
	src := testsupport.InDir("e2e-split")
	outDir := testsupport.OutDir(t, "e2e-split", "emit-provider-alias")
	a, err := pipeline.Analyze(src, pipeline.Options{Remainder: "monolith"})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	e := &emit.Emitter{SrcDir: src, OutDir: outDir, Graph: a.Graph, Place: a.Placement, Bound: a.Boundary}
	if _, err := e.Emit(); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	app := readFile(t, filepath.Join(outDir, "app", "main.tf"))

	// Three tls providers carved into app: the default plus the two aliases
	// (by_resource, by_module) whose configs reference cross-module producers.
	if got := strings.Count(app, `provider "tls"`); got != 3 {
		t.Errorf("app should carve 3 tls provider blocks (default + 2 aliased), got %d:\n%s", got, app)
	}
	for _, alias := range []string{`alias = "by_resource"`, `alias = "by_module"`} {
		if !strings.Contains(app, alias) {
			t.Errorf("app missing aliased tls provider %s:\n%s", alias, app)
		}
	}
	// The by_resource alias references a cross-module resource, rewritten to var.
	if strings.Contains(app, "random_pet.net_name.id") {
		t.Errorf("app provider still has raw cross-module ref random_pet.net_name.id:\n%s", app)
	}
	if !strings.Contains(app, "var.random_pet_net_name") {
		t.Errorf("app provider cross-module ref not rewritten to var.random_pet_net_name:\n%s", app)
	}
	// The by_module alias references a cross-module module output, rewritten to var.
	if strings.Contains(app, "${module.idgen.id}") {
		t.Errorf("app provider still has raw cross-module module ref module.idgen.id:\n%s", app)
	}
	if !strings.Contains(app, "var.module_idgen") {
		t.Errorf("app provider cross-module module ref not rewritten to var.module_idgen:\n%s", app)
	}
	// Resources keep their explicit aliased provider selection.
	for _, sel := range []string{"tls.by_resource", "tls.by_module"} {
		if !strings.Contains(app, sel) {
			t.Errorf("app lost provider selection %q:\n%s", sel, app)
		}
	}
}
