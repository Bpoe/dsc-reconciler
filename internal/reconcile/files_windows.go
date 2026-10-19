//go:build windows

package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func inputPath(path string) (*uint16, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(absolute, `\\?\`) {
		if strings.HasPrefix(absolute, `\\`) {
			absolute = `\\?\UNC\` + absolute[2:]
		} else {
			absolute = `\\?\` + absolute
		}
	}
	return windows.UTF16PtrFromString(absolute)
}

func openInputFile(path string) (*os.File, error) {
	name, err := inputPath(path)
	if err != nil {
		return nil, err
	}
	// Notifications can arrive before the producer closes its rename handle.
	// Delete sharing permits sidecar reads and directory identity checks
	// without obstructing atomic replacement or directory moves.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

func unavailableConfiguration(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_DELETE_PENDING) ||
		errors.Is(err, os.ErrNotExist)
}

func openProtectedFile(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := inputPath(path)
	if err != nil {
		return nil, err
	}
	// Compatible readers remain allowed; writers and delete/rename handles do not.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if unavailableConfiguration(err) {
		return nil, errStaleInput
	}
	if err != nil {
		return nil, &os.PathError{Op: "open protected configuration", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
