package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/schrieksoft/demonolith/internal/boundary"
)

// externalBoundary builds a boundary declaring the given names as external
// inputs on one module.
func externalBoundary(names ...string) *boundary.Result {
	b := &boundary.ModuleBoundary{Module: "m", Inputs: map[string]boundary.Input{}}
	for _, n := range names {
		b.Inputs[n] = boundary.Input{Name: n, External: true, SourceVar: n}
	}
	b.Inputs["threaded"] = boundary.Input{Name: "threaded", FromModule: "p", FromOutput: "o"}
	return &boundary.Result{Boundaries: map[string]*boundary.ModuleBoundary{"m": b}}
}

func TestCollectExternalInputs_Precedence(t *testing.T) {
	root := t.TempDir()
	// Auto-loaded files: terraform.tfvars, then *.auto.tfvars overrides it.
	if err := os.WriteFile(filepath.Join(root, "terraform.tfvars"),
		[]byte("region = \"base\"\nsize = 3\nflag = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "extra.auto.tfvars"),
		[]byte("region = \"auto\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Explicit --var-file overrides the auto-loaded values.
	vf := filepath.Join(root, "prod.tfvars")
	if err := os.WriteFile(vf, []byte("region = \"prod\"\nowner = \"karl\"\nthreaded = \"nope\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TF_VAR_owner", "env-owner")
	t.Setenv("TF_VAR_unrelated", "x")

	bound := externalBoundary("region", "size", "flag", "owner", "cli")
	vals, names, err := collectExternalInputs(root, bound, []string{vf}, []string{"cli=v", "owner=flag-owner"})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"region": "prod",       // var-file beats terraform.tfvars and *.auto.tfvars
		"size":   "3",          // number rendered bare
		"flag":   "true",       // bool rendered bare
		"owner":  "flag-owner", // --var beats TF_VAR_ beats var-file
		"cli":    "v",
	}
	for k, v := range want {
		if vals[k] != v {
			t.Errorf("%s = %q, want %q", k, vals[k], v)
		}
	}
	if _, ok := vals["threaded"]; ok {
		t.Error("a cross-module input name must not be collectable from tfvars")
	}
	if _, ok := vals["unrelated"]; ok {
		t.Error("an undeclared name must not be collected from TF_VAR_*")
	}
	if len(names) != len(want) {
		t.Errorf("resolved names = %v, want %d entries", names, len(want))
	}
}

func TestCollectExternalInputs_MissingVarFile(t *testing.T) {
	root := t.TempDir()
	bound := externalBoundary("region")
	if _, _, err := collectExternalInputs(root, bound, []string{filepath.Join(root, "absent.tfvars")}, nil); err == nil {
		t.Error("an explicit --var-file that does not exist must be an error, not a silent skip")
	}
}
