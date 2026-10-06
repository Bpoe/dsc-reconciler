package results

import (
	"context"
	"os"
	"testing"
)

func readShared(path string) ([]byte, error) { return os.ReadFile(path) }

func TestPrivatePermissions(t *testing.T) {
	writer := newTestWriter(t)
	result := testResult("a.yaml", "succeeded")
	if err := writer.Write(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	path, _ := writer.Destination(result.Configuration)
	for path, mode := range map[string]os.FileMode{writer.dir: 0700, path: 0600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s permissions: %o, want %o", path, info.Mode().Perm(), mode)
		}
	}
}
