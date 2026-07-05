package hclgraph

import (
	"path/filepath"
	"testing"
)

func TestParseDir_Fixture(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "monolith", "in")
	g, err := ParseDir(dir)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}

	want := []string{
		"random_uuid.vpc_id",
		"random_uuid.private_subnet_id",
		"random_uuid.database_id",
		"random_password.admin_password",
		"random_pet.environment",
		"time_sleep.wait_10s",
		"data.random_id.shared_token",
	}
	for _, w := range want {
		if _, ok := g.Nodes[w]; !ok {
			t.Errorf("missing node %q", w)
		}
	}

	// The cross-module edge that drives boundary computation: database_id
	// references private_subnet_id.
	db := g.Nodes["random_uuid.database_id"]
	if db == nil {
		t.Fatal("no database_id node")
	}
	if !hasRef(db, "random_uuid.private_subnet_id") {
		t.Errorf("database_id refs = %v, want private_subnet_id", db.Refs)
	}

	// depends_on refs are ordering-only, so they land in DependsOnOnly, not Refs.
	vpc := g.Nodes["random_uuid.vpc_id"]
	if hasRef(vpc, "time_sleep.wait_10s") {
		t.Errorf("vpc_id: depends_on ref should not be a value ref, got Refs=%v", vpc.Refs)
	}
	if !hasDependsOn(vpc, "time_sleep.wait_10s") {
		t.Errorf("vpc_id DependsOnOnly = %v, want time_sleep.wait_10s", vpc.DependsOnOnly)
	}
}

func hasDependsOn(n *Node, s string) bool {
	for _, r := range n.DependsOnOnly {
		if r.String() == s {
			return true
		}
	}
	return false
}

func hasRef(n *Node, s string) bool {
	for _, r := range n.Refs {
		if r.String() == s {
			return true
		}
	}
	return false
}
