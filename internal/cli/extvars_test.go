package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCollectVarProvenance_SourcesAndPrecedence: each later source wins and
// provenance names where the value came from.
func TestCollectVarProvenance_SourcesAndPrecedence(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "terraform.tfvars"), []byte("a = \"from-tfvars\"\nb = \"from-tfvars\"\nc = \"from-tfvars\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extra.auto.tfvars"), []byte("b = \"from-auto\"\nc = \"from-auto\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vf := filepath.Join(dir, "given.tfvars")
	if err := os.WriteFile(vf, []byte("c = \"from-var-file\"\nd = \"from-var-file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF_VAR_a", "from-env-loses")
	t.Setenv("TF_VAR_e", "from-env")

	rv, err := collectVarProvenance(dir, []string{vf}, []string{"d=from-flag"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"a": {"from-tfvars", "terraform.tfvars"},
		"b": {"from-auto", "extra.auto.tfvars"},
		"c": {"from-var-file", "--var-file " + vf},
		"d": {"from-flag", "--var"},
		"e": {"from-env", "TF_VAR_e (environment)"},
	}
	for name, w := range want {
		got, ok := rv[name]
		if !ok {
			t.Errorf("%s: missing", name)
			continue
		}
		if got.Value != w[0] || got.Source != w[1] {
			t.Errorf("%s: got %q from %q, want %q from %q", name, got.Value, got.Source, w[0], w[1])
		}
	}
}

// TestModuleVarDecls_DefaultDetection distinguishes required declarations
// from defaulted ones.
func TestModuleVarDecls_DefaultDetection(t *testing.T) {
	dir := t.TempDir()
	src := "variable \"required_one\" {\n  type = string\n}\n\nvariable \"defaulted\" {\n  type    = string\n  default = \"x\"\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "variables.tf"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	decls, err := moduleVarDecls(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(decls) != 2 || decls["required_one"] || !decls["defaulted"] {
		t.Errorf("decls = %v, want required_one=false, defaulted=true", decls)
	}
}
