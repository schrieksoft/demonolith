package transfer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMatchSnapcdModules(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "platform")
	recv := filepath.Join(base, "network")
	snap := filepath.Join(base, "snapcd")
	for _, d := range []string{src, recv, snap} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	main := `
variable "prefix" {
  type    = string
  default = ""
}

resource "snapcd_module" "platform" {
  name                = "platform"
  source_subdirectory = "platform"
}

resource "snapcd_module" "network" {
  name                = "network"
  source_subdirectory = "${var.prefix}roots/network"
}
`
	if err := os.WriteFile(filepath.Join(snap, "main.tf"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}

	mods, err := MatchSnapcdModules(snap, src, map[string]string{"../network": recv})
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if mods["source"] != "platform" || mods["../network"] != "network" {
		t.Fatalf("modules: %v", mods)
	}

	// A root with no matching module refuses.
	_, err = MatchSnapcdModules(snap, filepath.Join(base, "storage"), nil)
	if err == nil || !strings.Contains(err.Error(), "no snapcd_module") {
		t.Fatalf("missing module must refuse, got: %v", err)
	}
}

func TestSnapcdFileHCL(t *testing.T) {
	m := &Map{
		Remainder: "legacy",
		Receivers: map[string]Receiver{"../network": {}},
		CrossEdges: []CrossEdge{
			{Consumer: "legacy", Input: "vpc_id_result", Producer: "../network", Output: "vpc_id_result"},
		},
		OrderingEdges: []OrderingEdge{
			{Consumer: "../network", Producer: "legacy"},
		},
		Snapcd: &Snapcd{Dir: "snapcd", Modules: map[string]string{"source": "platform", "../network": "network"}},
	}
	hcl := SnapcdFileHCL(m)
	for _, want := range []string{
		`resource "snapcd_module_input_from_output" "platform_vpc_id_result"`,
		"module_id        = snapcd_module.platform.id",
		"output_module_id = snapcd_module.network.id",
		`output_name      = "vpc_id_result"`,
		`resource "snapcd_depends_on_module" "network_on_platform"`,
	} {
		if !strings.Contains(hcl, want) {
			t.Fatalf("snapcd file missing %q:\n%s", want, hcl)
		}
	}
	m.Snapcd = nil
	if SnapcdFileHCL(m) != "" {
		t.Fatal("no snapcd root -> no file")
	}
}
