package dsc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func startProcess(cmd *exec.Cmd) (func() error, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate daemon executable: %w", err)
	}
	cmd.Env = bundledEnvironment(cmd.Path, executable, cmd.Environ())
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

func bundledEnvironment(path, executable string, environment []string) []string {
	bundle := filepath.Join(filepath.Dir(executable), "dsc")
	if !strings.EqualFold(filepath.Clean(path), filepath.Join(bundle, "dsc.exe")) {
		return environment
	}
	// Honor DSC's explicit isolation mode rather than broadening its search.
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, "DSC_RESTRICTED_PATH") {
			return environment
		}
	}
	child := append([]string(nil), environment...)
	hasPath := false
	for i, entry := range child {
		key, value, _ := strings.Cut(entry, "=")
		switch {
		case strings.EqualFold(key, "PATH"):
			child[i] = key + "=" + bundle
			if value != "" {
				child[i] += ";" + value
			}
			hasPath = true
		case strings.EqualFold(key, "DSC_RESOURCE_PATH"):
			// DSC_RESOURCE_PATH replaces manifest discovery through PATH.
			// Retain custom locations and their priority, then add our bundle.
			child[i] = key + "="
			if value != "" {
				child[i] += value + ";"
			}
			child[i] += bundle
		}
	}
	if !hasPath {
		child = append(child, "PATH="+bundle)
	}
	return child
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
