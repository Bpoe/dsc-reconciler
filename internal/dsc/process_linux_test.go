package dsc

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func processStopped(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	// A killed orphan may remain a zombie until the host's init reaps it.
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	_, state, ok := strings.Cut(string(data), ") ")
	return ok && strings.HasPrefix(state, "Z")
}
