package reconcile

import (
	"os"
	"testing"
)

func TestUnreadableConfigurationWithMetadataFailsOnlyConfiguration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read files without permission bits")
	}
	dir := t.TempDir()
	path := writeInput(t, dir, "web.yaml", "metadata: {dscd: {operation: test}}\n")
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(path, 0600); err != nil {
			t.Error(err)
		}
	}()
	assertConfigurationFailure(t, dir, "web.yaml")
}
