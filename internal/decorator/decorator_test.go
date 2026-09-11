package decorator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scanFile(t *testing.T, name string) []BlockDecorators {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "monolith", "in", name)
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	bds, err := Scan(path, src)
	if err != nil {
		t.Fatalf("Scan(%s): %v", name, err)
	}
	return bds
}

func targetsFor(bds []BlockDecorators, addr string) []string {
	for _, bd := range bds {
		if bd.Addr == addr {
			var all []string
			for _, d := range bd.Decorators {
				all = append(all, d.Targets...)
			}
			return all
		}
	}
	return nil
}

func TestScan_SingleTarget(t *testing.T) {
	bds := scanFile(t, "networking.tf")
	got := targetsFor(bds, "random_uuid.vpc_id")
	if len(got) != 1 || got[0] != "networking" {
		t.Errorf("vpc_id targets = %v, want [networking]", got)
	}
	// undecorated resource -> no decorators (catchall handled later)
	if tw := targetsFor(bds, "time_sleep.wait_10s"); len(tw) != 0 {
		t.Errorf("wait_10s should have no decorators, got %v", tw)
	}
}

func TestScan_DataDecorator_IsHardError(t *testing.T) {
	src := []byte("# @demono:move networking\ndata \"random_id\" \"token\" {}\n")
	if _, err := Scan("data.tf", src); err == nil {
		t.Fatal("a decorator on a data block must be a hard error: data sources are placed automatically")
	}
}

func TestScan_Malformed_IsHardError(t *testing.T) {
	src := []byte("# @demono:mve networking\nresource \"random_pet\" \"x\" {}\n")
	if _, err := Scan("bad.tf", src); err == nil {
		t.Fatal("expected hard error on malformed decorator")
	}
}

func TestScan_MultiTargetResource_IsError(t *testing.T) {
	src := []byte("# @demono:move a\n# @demono:move b\nresource \"random_pet\" \"x\" {}\n")
	if _, err := Scan("bad.tf", src); err == nil {
		t.Fatal("expected error: resource with two targets")
	}
}

func TestScan_OrphanDecorator_IsError(t *testing.T) {
	src := []byte("# @demono:move a\n\nresource \"random_pet\" \"x\" {}\n")
	if _, err := Scan("orphan.tf", src); err == nil {
		t.Fatal("expected error: decorator not attached to a block")
	}
}

func TestScan_SplitAndTransferVerbs(t *testing.T) {
	src := []byte(`
# @demono:split networking
resource "random_uuid" "a" {}

# @demono:transfer
resource "random_uuid" "b" {}
`)
	bds, err := Scan("main.tf", src)
	if err != nil {
		t.Fatalf("split/transfer verbs must parse: %v", err)
	}
	byAddr := map[string]Decorator{}
	for _, bd := range bds {
		if len(bd.Decorators) > 0 {
			byAddr[bd.Addr] = bd.Decorators[0]
		}
	}
	if d := byAddr["random_uuid.a"]; d.Verb != VerbSplit || len(d.Targets) != 1 || d.Targets[0] != "networking" {
		t.Fatalf("split decorator: %+v", d)
	}
	if d := byAddr["random_uuid.b"]; d.Verb != VerbTransfer || len(d.Targets) != 0 {
		t.Fatalf("transfer decorator: %+v", d)
	}

	if _, err := Scan("main.tf", []byte("# @demono:transfer ../x\nresource \"random_uuid\" \"c\" {}\n")); err == nil || !strings.Contains(err.Error(), "--transfer-target") {
		t.Fatalf("transfer with a target must refuse naming the flag, got: %v", err)
	}
	if _, err := Scan("main.tf", []byte("# @demono:split\nresource \"random_uuid\" \"d\" {}\n")); err == nil || !strings.Contains(err.Error(), "requires a target") {
		t.Fatalf("split without a target must refuse, got: %v", err)
	}
	if _, err := Scan("main.tf", []byte("# @demono:relocate x\nresource \"random_uuid\" \"e\" {}\n")); err == nil || !strings.Contains(err.Error(), "unknown decorator verb") {
		t.Fatalf("unknown verb must refuse, got: %v", err)
	}
}
