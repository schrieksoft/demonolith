package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRoot(t *testing.T, main string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestAnalyze_OpVerbs: each operation accepts its own decorator verb plus the
// deprecated move, and rejects the other's with a message naming the fix.
func TestAnalyze_OpVerbs(t *testing.T) {
	split := writeRoot(t, "# @demono:split networking\nresource \"random_uuid\" \"a\" {}\n")
	a, err := Analyze(split, Options{})
	if err != nil || a.LegacyMove {
		t.Fatalf("split verb in a split: err=%v legacy=%v", err, a.LegacyMove)
	}

	legacy := writeRoot(t, "# @demono:move networking\nresource \"random_uuid\" \"a\" {}\n")
	a, err = Analyze(legacy, Options{})
	if err != nil || !a.LegacyMove {
		t.Fatalf("move verb must work and be flagged legacy: err=%v legacy=%v", err, a.LegacyMove)
	}

	if _, err := Analyze(writeRoot(t, "# @demono:transfer\nresource \"random_uuid\" \"a\" {}\n"), Options{}); err == nil || !strings.Contains(err.Error(), "@demono:split") {
		t.Fatalf("transfer verb in a split must refuse naming @demono:split, got: %v", err)
	}
	if _, err := Analyze(writeRoot(t, "# @demono:split x\nresource \"random_uuid\" \"a\" {}\n"), Options{Op: OpTransfer, TransferTarget: "../x"}); err == nil || !strings.Contains(err.Error(), "@demono:transfer") {
		t.Fatalf("split verb in a transfer must refuse naming @demono:transfer, got: %v", err)
	}
	if _, err := Analyze(writeRoot(t, "# @demono:transfer\nresource \"random_uuid\" \"a\" {}\n"), Options{Op: OpTransfer}); err == nil || !strings.Contains(err.Error(), "--transfer-target") {
		t.Fatalf("bare transfer without a target must refuse naming the flag, got: %v", err)
	}
	if _, err := Analyze(writeRoot(t, "# @demono:move ../y\nresource \"random_uuid\" \"a\" {}\n"), Options{Op: OpTransfer, TransferTarget: "../x"}); err == nil || !strings.Contains(err.Error(), "one receiver") {
		t.Fatalf("move target disagreeing with --transfer-target must refuse, got: %v", err)
	}
}
