package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type lifecycleOutput struct {
	buffer  bytes.Buffer
	started chan struct{}
	ready   bool
}

func (o *lifecycleOutput) Write(p []byte) (int, error) {
	n, err := o.buffer.Write(p)
	if !o.ready && bytes.Contains(o.buffer.Bytes(), []byte("dscd started")) {
		o.ready = true
		close(o.started)
	}
	return n, err
}

func TestForegroundSIGTERM(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "input with spaces")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-config-dir", input, "-results-dir",
		filepath.Join(root, "results with spaces"), "-dsc-path", executable)
	cmd.Env = append(os.Environ(), "DSCD_TEST_DAEMON=1")
	output := &lifecycleOutput{started: make(chan struct{})}
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-output.started:
	case <-ctx.Done():
		_ = cmd.Wait()
		t.Fatalf("daemon failed to start: %s", output.buffer.String())
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		cancel()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("shutdown failed: %v (%s)", err, output.buffer.String())
	}
	if !bytes.Contains(output.buffer.Bytes(), []byte("dscd stopped")) {
		t.Fatal("normal shutdown was not logged")
	}
}
