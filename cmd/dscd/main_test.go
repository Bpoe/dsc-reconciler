package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dsc-reconciler/internal/dsc"
)

func TestMain(m *testing.M) {
	if os.Getenv("DSCD_TEST_RESULT_PROCESS") == "1" {
		if len(os.Args) != 7 || os.Args[1] != "config" || os.Args[2] != "set" {
			os.Exit(2)
		}
		document, err := os.ReadFile(os.Args[4])
		if err != nil {
			os.Exit(3)
		}
		if string(document) == "invalid output" {
			fmt.Print("invalid JSON")
		} else {
			fmt.Print(`{"metadata":{},"results":[],"messages":[],"hadErrors":false}`)
		}
		os.Exit(0)
	}
	if os.Getenv("DSCD_TEST_DAEMON") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestEndToEndPublicationAndContinuation(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "input with spaces")
	output := filepath.Join(root, "results with spaces")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"10-failure.yaml": "invalid output", "20-success.json": "opaque input"} {
		if err := os.WriteFile(filepath.Join(input, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSCD_TEST_RESULT_PROCESS", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = run(ctx, []string{"-config-dir", input, "-results-dir", output, "-dsc-path", executable, "-interval", "1h"},
			io.Discard, slog.New(slog.NewJSONHandler(io.Discard, nil)), func() {})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(output, "20-success.json.result.json")); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-ticker.C:
		case <-done:
			t.Fatalf("daemon exited before publishing: %v", runErr)
		case <-ctx.Done():
			t.Fatal("results were not published")
		}
	}
	cancel()
	<-done
	if runErr != nil {
		t.Fatal(runErr)
	}
	for name, outcome := range map[string]string{"10-failure.yaml": "failed", "20-success.json": "succeeded"} {
		data, err := os.ReadFile(filepath.Join(output, name+".result.json"))
		if err != nil {
			t.Fatal(err)
		}
		var result dsc.Result
		if err := json.Unmarshal(data, &result); err != nil || result.Configuration != name || result.Outcome != outcome {
			t.Fatalf("unexpected result %s: %s (%v)", name, data, err)
		}
	}
}

func TestStartupAndShutdown(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "input with spaces")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-config-dir", input, "-results-dir", filepath.Join(root, "results with spaces"), "-dsc-path", executable}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	ready := false
	err = boundedRun(ctx, func() error {
		return run(ctx, args, io.Discard, logger, func() { ready = true; cancel() })
	}, time.Second)
	if err != nil || !ready {
		t.Fatalf("startup/shutdown: ready=%t, %v", ready, err)
	}
	if err := run(context.Background(), []string{"-interval", "0"}, io.Discard, logger, func() {}); err == nil {
		t.Fatal("accepted invalid startup")
	}
	if err := run(context.Background(), []string{"-help"}, io.Discard, logger, func() {}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help: %v", err)
	}
}

func TestShutdownBound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release := make(chan struct{})
	finished := make(chan struct{})
	err := boundedRun(ctx, func() error { defer close(finished); <-release; return nil }, 10*time.Millisecond)
	close(release)
	<-finished
	if err == nil {
		t.Fatal("blocked shutdown was reported as successful")
	}
}
