package reconcile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsRecursiveRootWatchAllowsParentRename(t *testing.T) {
	root := t.TempDir()
	input(t, root, "parent/child/web.yaml")
	name, err := windows.UTF16PtrFromString(root)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(event)
	overlapped := windows.Overlapped{HEvent: event}
	buffer := make([]uint32, 16384) // ReadDirectoryChangesW requires DWORD alignment.
	err = windows.ReadDirectoryChanges(handle, (*byte)(unsafe.Pointer(&buffer[0])), uint32(len(buffer)*4),
		true, windows.FILE_NOTIFY_CHANGE_DIR_NAME|windows.FILE_NOTIFY_CHANGE_FILE_NAME, nil, &overlapped, 0)
	if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
		t.Fatal(err)
	}
	defer func() {
		err := windows.CancelIoEx(handle, &overlapped)
		if err != nil && !errors.Is(err, windows.ERROR_NOT_FOUND) {
			t.Error(err)
		}
		var size uint32
		err = windows.GetOverlappedResult(handle, &overlapped, &size, true)
		if err != nil && !errors.Is(err, windows.ERROR_OPERATION_ABORTED) {
			t.Error(err)
		}
	}()
	if err := os.Rename(filepath.Join(root, "parent"), filepath.Join(root, "renamed")); err != nil {
		t.Fatalf("root-only subtree watch blocked parent rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "renamed", "child", "web.yaml")); err != nil {
		t.Fatalf("descendant did not move with parent: %v", err)
	}
	state, err := windows.WaitForSingleObject(event, 5000)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("rename notification: state=%d, error=%v", state, err)
	}
	var size uint32
	if err := windows.GetOverlappedResult(handle, &overlapped, &size, false); err != nil || size == 0 {
		t.Fatalf("recursive watch did not receive rename: bytes=%d, error=%v", size, err)
	}
}

func renameInput(from, to string) error {
	source, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source, target, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
