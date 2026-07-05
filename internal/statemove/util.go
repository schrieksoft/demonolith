package statemove

import (
	"io"
	"os"
	"os/exec"
)

// copyFile copies src to dst, creating or truncating dst.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }() // read-only: close error is not meaningful

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	// Close explicitly so a flush error surfaces; the deferred close is a
	// safety net for the early-return paths and is a no-op after a clean close.
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// lookPath resolves an executable name to an absolute path.
func lookPath(name string) (string, error) {
	return exec.LookPath(name)
}
