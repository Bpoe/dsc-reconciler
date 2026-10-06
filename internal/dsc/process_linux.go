package dsc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func startProcess(cmd *exec.Cmd) (func() error, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	kill := func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.Cancel = kill
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() error {
		err := kill()
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}, nil
}
