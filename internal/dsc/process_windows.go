package dsc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func startProcess(cmd *exec.Cmd) (func() error, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create process job: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return nil, errors.Join(err, windows.CloseHandle(job))
	}
	// Do not let DSC spawn descendants before job assignment.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW}
	cmd.Cancel = func() error {
		jobErr := windows.TerminateJobObject(job, 1)
		killErr := cmd.Process.Kill() // Covers cancellation before assignment.
		if errors.Is(killErr, os.ErrProcessDone) {
			killErr = nil
		}
		return errors.Join(jobErr, killErr)
	}
	if err = cmd.Start(); err != nil {
		return nil, errors.Join(err, windows.CloseHandle(job))
	}
	if err = assignAndResume(job, uint32(cmd.Process.Pid)); err != nil {
		killErr := cmd.Process.Kill()
		waitErr := cmd.Wait()
		return nil, errors.Join(err, killErr, waitErr, windows.CloseHandle(job))
	}
	return func() error { return windows.CloseHandle(job) }, nil
}

func assignAndResume(job windows.Handle, pid uint32) error {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return fmt.Errorf("open suspended DSC: %w", err)
	}
	err = windows.AssignProcessToJobObject(job, process)
	closeErr := windows.CloseHandle(process)
	if err != nil || closeErr != nil {
		return fmt.Errorf("assign DSC job: %w", errors.Join(err, closeErr))
	}
	// os/exec closes the primary thread handle; locate it while still suspended.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot DSC thread: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return fmt.Errorf("open DSC thread: %w", err)
		}
		_, resumeErr := windows.ResumeThread(thread)
		return errors.Join(resumeErr, windows.CloseHandle(thread))
	}
	return fmt.Errorf("find suspended DSC thread: %w", err)
}
