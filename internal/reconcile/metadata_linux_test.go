package reconcile

import (
	"os"
	"testing"
)

func TestUnreadableMetadataFailsOnlyConfiguration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read files without permission bits")
	}
	dir := t.TempDir()
	path := writeMetadata(t, dir, `{"operation":"test"}`)
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(path, 0600); err != nil {
			t.Error(err)
		}
	}()
	assertMetadataFailure(t, dir)
}
