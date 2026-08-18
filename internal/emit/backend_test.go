package emit_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/schrieksoft/demonolith/internal/emit"
)

// renderRootTF builds the module's root.tf content the way emit composes it:
// a terraform{} block wrapping the derived backend block.
func renderRootTF(t *testing.T, b *emit.BackendBlock, module string) string {
	t.Helper()
	blk, err := b.BackendHCL(module)
	if err != nil {
		t.Fatal(err)
	}
	f := hclwrite.NewEmptyFile()
	tfb := f.Body().AppendNewBlock("terraform", nil)
	tfb.Body().AppendBlock(blk)
	return string(hclwrite.Format(f.Bytes()))
}

func TestDeriveLocation(t *testing.T) {
	cases := []struct{ in, module, want string }{
		{"terraform.tfstate", "networking", "terraform-networking.tfstate"},
		{"prod/terraform.tfstate", "app", "prod/terraform-app.tfstate"},
		{"envs/prod", "networking", "envs/prod-networking"},
		{"https://host/api/state/monolith", "db", "https://host/api/state/monolith-db"},
		{"https://host/api/state/monolith/lock", "db", "https://host/api/state/monolith-db/lock"},
		{"https://host/api/state/monolith/unlock", "db", "https://host/api/state/monolith-db/unlock"},
	}
	for _, c := range cases {
		if got := emit.DeriveLocation(c.in, c.module); got != c.want {
			t.Errorf("DeriveLocation(%q, %q) = %q, want %q", c.in, c.module, got, c.want)
		}
	}
}

func writeBackendFixture(t *testing.T, hcl string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "root.tf"), []byte(hcl), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestParseBackend_DeriveAndWrite(t *testing.T) {
	dir := writeBackendFixture(t, `
terraform {
  backend "s3" {
    bucket = "my-bucket"
    key    = "prod/terraform.tfstate"
    region = "eu-west-1"
  }
}
`)
	b, err := emit.ParseBackend(dir)
	if err != nil || b == nil {
		t.Fatalf("ParseBackend: %v, %v", b, err)
	}
	mono, byModule, err := b.DerivedLocations([]string{"networking", "app"})
	if err != nil {
		t.Fatal(err)
	}
	if mono != "prod/terraform.tfstate" || byModule["networking"] != "prod/terraform-networking.tfstate" {
		t.Errorf("derived = %q / %v", mono, byModule)
	}

	s := renderRootTF(t, b, "networking")
	for _, want := range []string{`backend "s3"`, `key    = "prod/terraform-networking.tfstate"`, `bucket = "my-bucket"`, `region = "eu-west-1"`} {
		if !strings.Contains(s, want) {
			t.Errorf("root.tf missing %q:\n%s", want, s)
		}
	}
}

func TestParseBackend_HTTPDerivesAllThree(t *testing.T) {
	dir := writeBackendFixture(t, `
terraform {
  backend "http" {
    address        = "http://host/api/state/mono"
    lock_address   = "http://host/api/state/mono/lock"
    unlock_address = "http://host/api/state/mono/unlock"
  }
}
`)
	b, err := emit.ParseBackend(dir)
	if err != nil || b == nil {
		t.Fatal(err)
	}
	s := renderRootTF(t, b, "db")
	for _, want := range []string{"state/mono-db\"", "mono-db/lock\"", "mono-db/unlock\""} {
		if !strings.Contains(s, want) {
			t.Errorf("root.tf missing derived %q:\n%s", want, s)
		}
	}
}

func TestParseBackend_Refusals(t *testing.T) {
	// Unsupported type (swift was removed from the lineage before the fork).
	dir := writeBackendFixture(t, "terraform {\n  backend \"swift\" {\n    container = \"x\"\n  }\n}\n")
	b, err := emit.ParseBackend(dir)
	if err != nil || b == nil {
		t.Fatal(err)
	}
	if _, _, err := b.DerivedLocations([]string{"a"}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("unsupported type must refuse, got: %v", err)
	}

	// A cloud block is workspace-driven, not a derivable location.
	dir = writeBackendFixture(t, "terraform {\n  cloud {\n    organization = \"acme\"\n  }\n}\n")
	b, err = emit.ParseBackend(dir)
	if err != nil || b == nil || b.Type != "cloud" {
		t.Fatalf("cloud block must surface as type cloud, got %+v, %v", b, err)
	}
	if _, _, err := b.DerivedLocations([]string{"a"}); err == nil || !strings.Contains(err.Error(), "cloud block") {
		t.Errorf("cloud block must refuse derivation, got: %v", err)
	}

	// remote in prefix mode maps CLI workspaces; only name mode derives.
	dir = writeBackendFixture(t, "terraform {\n  backend \"remote\" {\n    organization = \"acme\"\n    workspaces {\n      prefix = \"mono-\"\n    }\n  }\n}\n")
	b, err = emit.ParseBackend(dir)
	if err != nil || b == nil {
		t.Fatal(err)
	}
	if _, _, err := b.DerivedLocations([]string{"a"}); err == nil || !strings.Contains(err.Error(), "prefix") {
		t.Errorf("remote prefix mode must refuse, got: %v", err)
	}

	// Location attribute absent from HCL (supplied out-of-band).
	dir = writeBackendFixture(t, "terraform {\n  backend \"s3\" {\n    bucket = \"b\"\n  }\n}\n")
	b, err = emit.ParseBackend(dir)
	if err != nil || b == nil {
		t.Fatal(err)
	}
	if _, _, err := b.DerivedLocations([]string{"a"}); err == nil || !strings.Contains(err.Error(), "neither in the HCL block nor") {
		t.Errorf("missing location attr (and no resolved config) must refuse, got: %v", err)
	}

	// No backend at all.
	dir = writeBackendFixture(t, "resource \"random_pet\" \"x\" {}\n")
	b, err = emit.ParseBackend(dir)
	if err != nil || b != nil {
		t.Errorf("no backend must return nil, got %v, %v", b, err)
	}
}

func TestBackend_ResolvedConfigFallbackAndEnv(t *testing.T) {
	dir := writeBackendFixture(t, "terraform {\n  backend \"http\" {}\n}\n")
	// Simulate an init'd root: resolved config with locations and credentials.
	tfDir := filepath.Join(dir, ".terraform")
	if err := os.MkdirAll(tfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved := `{"backend":{"type":"http","config":{
		"address":"http://host/api/state/mono",
		"lock_address":"http://host/api/state/mono/lock",
		"unlock_address":"http://host/api/state/mono/unlock",
		"username":"default","password":"hunter2","lock_method":"POST"}}}`
	if err := os.WriteFile(filepath.Join(tfDir, "terraform.tfstate"), []byte(resolved), 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := emit.ParseBackend(dir)
	if err != nil || b == nil {
		t.Fatalf("ParseBackend: %v, %v", b, err)
	}
	mono, byModule, err := b.DerivedLocations([]string{"app"})
	if err != nil {
		t.Fatalf("locations must fall back to resolved config: %v", err)
	}
	if mono != "http://host/api/state/mono" || byModule["app"] != "http://host/api/state/mono-app" {
		t.Errorf("derived = %q / %v", mono, byModule)
	}

	modDir := t.TempDir()
	bt := renderRootTF(t, b, "app")
	if !strings.Contains(bt, "state/mono-app\"") || strings.Contains(bt, "hunter2") {
		t.Errorf("root.tf must carry derived locations and never credentials:\n%s", bt)
	}

	wrote, err := emit.WriteEnvFile(modDir, b.CredentialEnv())
	if err != nil || !wrote {
		t.Fatalf("WriteEnvFile: wrote=%v err=%v", wrote, err)
	}
	envB, err := os.ReadFile(filepath.Join(modDir, "demono.env"))
	if err != nil {
		t.Fatal(err)
	}
	env := string(envB)
	if !strings.Contains(env, "TF_HTTP_USERNAME='default'") || !strings.Contains(env, "TF_HTTP_PASSWORD='hunter2'") {
		t.Errorf(".env missing mapped credentials:\n%s", env)
	}
	info, _ := os.Stat(filepath.Join(modDir, "demono.env"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf(".env mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestParseBackend_AllTypes covers derivation for every supported type's
// primary location attribute.
func TestParseBackend_AllTypes(t *testing.T) {
	cases := []struct {
		hcl, wantMono, wantA string
	}{
		{"terraform {\n  backend \"cos\" {\n    bucket = \"b\"\n    key = \"prod/terraform.tfstate\"\n  }\n}\n", "prod/terraform.tfstate", "prod/terraform-a.tfstate"},
		{"terraform {\n  backend \"oss\" {\n    bucket = \"b\"\n    key = \"prod/terraform.tfstate\"\n  }\n}\n", "prod/terraform.tfstate", "prod/terraform-a.tfstate"},
		{"terraform {\n  backend \"kubernetes\" {\n    secret_suffix = \"mono\"\n  }\n}\n", "mono", "mono-a"},
		{"terraform {\n  backend \"pg\" {\n    schema_name = \"tf_mono\"\n  }\n}\n", "tf_mono", "tf_mono-a"},
		{"terraform {\n  backend \"remote\" {\n    organization = \"acme\"\n    workspaces {\n      name = \"mono\"\n    }\n  }\n}\n", "mono", "mono-a"},
	}
	for _, tc := range cases {
		dir := writeBackendFixture(t, tc.hcl)
		b, err := emit.ParseBackend(dir)
		if err != nil || b == nil {
			t.Fatalf("ParseBackend: %v, %v", b, err)
		}
		mono, byMod, err := b.DerivedLocations([]string{"a"})
		if err != nil {
			t.Fatalf("type %s: %v", b.Type, err)
		}
		if mono != tc.wantMono || byMod["a"] != tc.wantA {
			t.Errorf("type %s: got %q/%q, want %q/%q", b.Type, mono, byMod["a"], tc.wantMono, tc.wantA)
		}
	}
}

// TestParseBackend_RemoteWritesNestedWorkspaces asserts the emitted root.tf
// carries the derived name inside a workspaces block, not as a flat attribute.
func TestParseBackend_RemoteWritesNestedWorkspaces(t *testing.T) {
	dir := writeBackendFixture(t, "terraform {\n  backend \"remote\" {\n    hostname = \"tfe.example.com\"\n    organization = \"acme\"\n    workspaces {\n      name = \"mono\"\n    }\n  }\n}\n")
	b, err := emit.ParseBackend(dir)
	if err != nil || b == nil {
		t.Fatal(err)
	}
	s := renderRootTF(t, b, "networking")
	for _, want := range []string{"workspaces {", `name = "mono-networking"`, `organization = "acme"`, `hostname     = "tfe.example.com"`} {
		if !strings.Contains(s, want) {
			t.Errorf("root.tf missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "workspaces.name") {
		t.Errorf("nested location leaked as flat attribute:\n%s", s)
	}
}

// TestRequiredProviderNames reads the local names out of the source root.
func TestRequiredProviderNames(t *testing.T) {
	dir := writeBackendFixture(t, "terraform {\n  required_providers {\n    random = {\n      source = \"hashicorp/random\"\n    }\n    snapcd = {\n      source = \"registry.terraform.io/schrieksoft/snapcd\"\n    }\n  }\n}\n")
	names, err := emit.RequiredProviderNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "random" || names[1] != "snapcd" {
		t.Errorf("names = %v, want [random snapcd]", names)
	}
}
