// Package testsupport provides helpers for tests that need a real, applied
// Terraform state to carve and validate. It is intentionally not build-tagged
// so it can be imported by any _test package; callers guard on binary
// availability via RequireEngine.
package testsupport

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hashicorp/terraform-exec/tfexec"
)

// EnginePath resolves the terraform (or tofu) binary, preferring $DEMO_TF_EXEC,
// then terraform, then tofu. It returns "" if none is found.
func EnginePath() string {
	if p := os.Getenv("DEMO_TF_EXEC"); p != "" {
		return p
	}
	for _, name := range []string{"terraform", "tofu"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// RequireEngine skips the test if no terraform/tofu binary is available.
func RequireEngine(t *testing.T) string {
	t.Helper()
	p := EnginePath()
	if p == "" {
		t.Skip("no terraform/tofu binary found; set DEMO_TF_EXEC to run state tests")
	}
	return p
}

// OutDir returns testdata/<fixture>/out/<slug>, wiped and freshly created. Every
// test writes its artifacts here instead of a temp dir, so outputs are
// inspectable after a run and each test owns a non-conflicting subfolder. The
// path is resolved relative to the calling package's testdata (../../testdata).
func OutDir(t *testing.T, fixture, slug string) string {
	t.Helper()
	rel := filepath.Join("..", "..", "testdata", fixture, "out", slug)
	// Absolute: tfexec runs rooted at the module dir, so any state/out paths
	// derived from this must not be interpreted relative to that root.
	dir, err := filepath.Abs(rel)
	if err != nil {
		t.Fatalf("abs out dir %s: %v", rel, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("clean out dir %s: %v", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir out dir %s: %v", dir, err)
	}
	return dir
}

// CopyInto copies a fixture's *.tf files into dst (created if needed) and
// returns dst, recursing into subdirectories so local child-module source dirs
// (e.g. modules/idgen) travel with the root. Terraform working-dir artifacts
// (.terraform, lock files, state) are skipped so apply starts clean.
func CopyInto(t *testing.T, dst, src string) string {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if name == ".terraform" {
				continue
			}
			CopyInto(t, filepath.Join(dst, name), filepath.Join(src, name))
			continue
		}
		if filepath.Ext(name) != ".tf" {
			continue // skip lock files, state, tfvars from the committed fixture
		}
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), b, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dst
}

// InDir is the absolute path to a fixture's in/ source folder.
func InDir(fixture string) string {
	rel := filepath.Join("..", "..", "testdata", fixture, "in")
	if abs, err := filepath.Abs(rel); err == nil {
		return abs
	}
	return rel
}

// ApplyRoot inits and applies the root at dir with a local backend, producing a
// real terraform.tfstate. Returns the exec path used.
func ApplyRoot(t *testing.T, dir string) string {
	t.Helper()
	execPath := RequireEngine(t)
	tf, err := tfexec.NewTerraform(dir, execPath)
	if err != nil {
		t.Fatalf("new tf: %v", err)
	}
	ctx := context.Background()
	if err := tf.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := tf.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return execPath
}
