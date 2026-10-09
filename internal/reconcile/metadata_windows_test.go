package reconcile

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestUnreadableConfigurationWithMetadataFailsOnlyConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := writeInput(t, dir, "web.yaml", "metadata: {dscd: {operation: test}}\n")
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := windows.CloseHandle(handle); err != nil {
			t.Error(err)
		}
	}()
	assertConfigurationFailure(t, dir, "web.yaml")
}
