// Package bodyspooltest holds test helpers for the body spool.
package bodyspooltest

import (
	"os"
	"path/filepath"
	"testing"
)

// CgroupDir returns a cgroup v2 stand-in without a memory limit (memory.max "max") for
// bodyspool.NewRuntime in tests that do not run inside a memory-limited cgroup.
func CgroupDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	for name, v := range map[string]string{"memory.max": "max\n", "memory.current": "0\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
