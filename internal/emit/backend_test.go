package emit_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schrieksoft/demonolith/internal/emit"
)

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

	modDir := t.TempDir()
	if err := b.WriteBackendTF(modDir, "networking"); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(modDir, "backend.tf"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`backend "s3"`, `key    = "prod/terraform-networking.tfstate"`, `bucket = "my-bucket"`, `region = "eu-west-1"`} {
		if !strings.Contains(s, want) {
			t.Errorf("backend.tf missing %q:\n%s", want, s)
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
	modDir := t.TempDir()
	if err := b.WriteBackendTF(modDir, "db"); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(filepath.Join(modDir, "backend.tf"))
	s := string(out)
	for _, want := range []string{"state/mono-db\"", "mono-db/lock\"", "mono-db/unlock\""} {
		if !strings.Contains(s, want) {
			t.Errorf("backend.tf missing derived %q:\n%s", want, s)
		}
	}
}

func TestParseBackend_Refusals(t *testing.T) {
	// Unsupported type.
	dir := writeBackendFixture(t, "terraform {\n  backend \"oss\" {\n    key = \"x\"\n  }\n}\n")
	b, err := emit.ParseBackend(dir)
	if err != nil || b == nil {
		t.Fatal(err)
	}
	if _, _, err := b.DerivedLocations([]string{"a"}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("unsupported type must refuse, got: %v", err)
	}

	// Location attribute absent from HCL (supplied out-of-band).
	dir = writeBackendFixture(t, "terraform {\n  backend \"s3\" {\n    bucket = \"b\"\n  }\n}\n")
	b, err = emit.ParseBackend(dir)
	if err != nil || b == nil {
		t.Fatal(err)
	}
	if _, _, err := b.DerivedLocations([]string{"a"}); err == nil || !strings.Contains(err.Error(), "not a plain string") {
		t.Errorf("missing location attr must refuse, got: %v", err)
	}

	// No backend at all.
	dir = writeBackendFixture(t, "resource \"random_pet\" \"x\" {}\n")
	b, err = emit.ParseBackend(dir)
	if err != nil || b != nil {
		t.Errorf("no backend must return nil, got %v, %v", b, err)
	}
}
