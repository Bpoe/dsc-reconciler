package dsc

import (
	"errors"

	"golang.org/x/sys/windows"
)

func processStopped(pid int) bool {
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(process)
	state, err := windows.WaitForSingleObject(process, 0)
	return err == nil && state == windows.WAIT_OBJECT_0
}
