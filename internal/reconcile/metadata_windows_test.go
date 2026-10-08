package reconcile

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestUnreadableMetadataFailsOnlyConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := writeMetadata(t, dir, `{"operation":"test"}`)
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
	assertMetadataFailure(t, dir)
}
