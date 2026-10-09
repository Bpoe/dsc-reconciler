package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
	"github.com/Bpoe/dsc-reconciler/internal/reconcile"
	"github.com/Bpoe/dsc-reconciler/internal/results"
)

func TestMain(m *testing.M) {
	if os.Getenv("DSCD_TEST_RESULT_PROCESS") == "1" {
		os.Exit(resultServer())
	}
	if os.Getenv("DSCD_TEST_DAEMON") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestVersionCommand(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"--version", "-version"} {
		t.Run(arg, func(t *testing.T) {
			root := t.TempDir()
			resultsDir := filepath.Join(root, "results")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, arg,
				"-config-dir", filepath.Join(root, "missing-input"),
				"-results-dir", resultsDir, "-dsc-path", filepath.Join(root, "missing-dsc"))
			cmd.Env = append(os.Environ(), "DSCD_TEST_DAEMON=1")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("version command: %v (%s)", err, stderr.String())
			}
			if stdout.String() != "dscd "+version+"\n" || stderr.Len() != 0 {
				t.Fatalf("stdout=%q, stderr=%q", stdout.String(), stderr.String())
			}
			if _, err := os.Stat(resultsDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("version touched results directory: %v", err)
			}
		})
	}
}

func TestVersionOutputFailure(t *testing.T) {
	output, err := os.CreateTemp(t.TempDir(), "closed-output")
	if err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	err = run(context.Background(), []string{"--version"}, output, io.Discard, logger, func() {
		t.Fatal("version started the daemon")
	})
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("version output failure: %v", err)
	}
}

func TestRealProcessPerPassAndRecovery(t *testing.T) {
	for _, test := range []struct {
		mode    string
		servers int
		outcome string
	}{
		{"success", 1, "succeeded"}, {"normal failure", 1, "failed"},
		{"invalid output", 2, "failed"}, {"exit", 2, "failed"}, {"hang", 2, "canceled"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "inputs")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			for name, text := range map[string]string{
				"a.yaml":            fmt.Sprintf(`{"mode":%q,"resources":[]}`, test.mode),
				"b.yaml":            `{"mode":"test drift","metadata":{"dscd":{"operation":"test","future":[null,true,{"secret":"PRIVATE-METADATA"}]}},"resources":[]}`,
				"c.json":            `{"mode":"success","metadata":{"dscd":{"operation":"set"}},"resources":[]}`,
				"b.parameters.json": "private-sidecar-value",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			lifetime := filepath.Join(root, "lifetimes")
			t.Setenv("DSCD_TEST_RESULT_PROCESS", "1")
			t.Setenv("DSCD_SERVER_LIFETIME", lifetime)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			client := dsc.NewClient(executable, 2*time.Second)
			writer, err := results.NewWriter(filepath.Join(root, "results"))
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			loop := reconcile.New(dir, time.Second, func(ctx context.Context) (reconcile.DSC, error) { return client.Start(ctx) }, writer, slog.New(slog.NewJSONHandler(&logs, nil)))
			var previousPID int
			for pass := 1; pass <= 2; pass++ {
				if err := loop.Pass(context.Background()); err != nil {
					t.Fatal(err)
				}
				var pids []int
				for _, name := range []string{"a.yaml", "b.yaml", "c.json"} {
					path, _ := writer.Destination(name)
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var result dsc.Result
					if err := json.Unmarshal(data, &result); err != nil {
						t.Fatal(err)
					}
					expected := "succeeded"
					if name == "a.yaml" {
						expected = test.outcome
					}
					if result.Outcome != expected || result.InputHash == "" {
						t.Fatalf("%s: %+v", name, result)
					}
					operation := dsc.OperationSet
					if name == "b.yaml" {
						operation = dsc.OperationTest
						if !strings.Contains(string(result.DSCResult), `"inDesiredState":false`) {
							t.Fatal("lost test drift result")
						}
					}
					if result.Operation == nil || *result.Operation != operation {
						t.Fatalf("persisted operation: %+v", result)
					}
					if result.Outcome == "succeeded" {
						if result.ExitCode != nil || result.Stderr != "" {
							t.Fatal("fabricated process diagnostics")
						}
						var payload struct {
							Metadata struct {
								PID int `json:"serverPID"`
							} `json:"metadata"`
						}
						if err := json.Unmarshal(result.DSCResult, &payload); err != nil {
							t.Fatal(err)
						}
						pids = append(pids, payload.Metadata.PID)
					}
				}
				if pids[len(pids)-1] != pids[len(pids)-2] || pids[len(pids)-1] == previousPID {
					t.Fatalf("session not reused within pass or retained between passes: %v", pids)
				}
				previousPID = pids[len(pids)-1]
				data, err := os.ReadFile(lifetime)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(string(data), "start ") != pass*test.servers {
					t.Fatalf("unexpected server count: %s", data)
				}
				if test.servers == 1 && strings.Count(string(data), "stop ") != pass {
					t.Fatalf("server didn't stop after pass: %s", data)
				}
				if strings.Contains(logs.String(), "private-sidecar-value") || strings.Contains(logs.String(), "PRIVATE-METADATA") {
					t.Fatal("input or JSON-RPC diagnostic leaked to routine logs")
				}
			}
		})
	}
}

func resultServer() int {
	if len(os.Args) != 2 || os.Args[1] != "server" {
		return 2
	}
	if path := os.Getenv("DSCD_SERVER_LIFETIME"); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return 3
		}
		fmt.Fprintf(f, "start %d\n", os.Getpid())
		f.Close()
		defer func() {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err == nil {
				fmt.Fprintf(f, "stop %d\n", os.Getpid())
				f.Close()
			}
		}()
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req struct {
			ID     uint64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Arguments struct {
					Operation     dsc.Operation `json:"operation"`
					Configuration string        `json:"configuration"`
					Parameters    *string       `json:"parameters"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			return 4
		}
		if req.Method == "notifications/initialized" {
			continue
		}
		if req.Method == "initialize" {
			if os.Getenv("DSCD_TEST_INIT_FAILURE") == "1" {
				fmt.Println("invalid initialize response")
				continue
			}
			encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]string{"name": "fake", "version": "1"},
			}})
			continue
		}
		var document struct {
			Mode string `json:"mode"`
		}
		if json.Unmarshal([]byte(req.Params.Arguments.Configuration), &document) != nil {
			return 8
		}
		switch document.Mode {
		case "invalid output":
			fmt.Println("invalid JSON")
			continue
		case "normal failure":
			encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "private-sidecar-value"}})
			continue
		case "exit":
			return 7
		case "hang":
			for {
				time.Sleep(time.Hour)
			}
		}
		resourceResults := []any{}
		if document.Mode == "test drift" {
			if req.Params.Arguments.Operation != dsc.OperationTest ||
				req.Params.Arguments.Configuration != `{"mode":"test drift","metadata":{"dscd":{"operation":"test","future":[null,true,{"secret":"PRIVATE-METADATA"}]}},"resources":[]}` {
				return 6
			}
			resourceResults = append(resourceResults, map[string]any{"result": map[string]any{"inDesiredState": false}})
		} else if req.Params.Arguments.Operation != dsc.OperationSet {
			return 6
		}
		if req.Params.Arguments.Parameters != nil && *req.Params.Arguments.Parameters != "private-sidecar-value" {
			return 5
		}
		encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
			"structuredContent": map[string]any{"result": map[string]any{"metadata": map[string]any{"serverPID": os.Getpid()}, "results": resourceResults, "messages": []any{}, "hadErrors": false}},
		}})
	}
	return 0
}

func TestEndToEndPublicationAndContinuation(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "input with spaces")
	output := filepath.Join(root, "results with spaces")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"05-metadata.yaml":           "metadata:\n  dscd:\n    operation: PRIVATE-METADATA\nresources: []\n",
		"10-failure.yaml":            `{"mode":"invalid output","resources":[]}`,
		"20-success.json":            `{"resources":[]}`,
		"20-success.parameters.yaml": "private-sidecar-value",
	} {
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
			io.Discard, io.Discard, slog.New(slog.NewJSONHandler(io.Discard, nil)), func() {})
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
	for name, outcome := range map[string]string{"05-metadata.yaml": "failed", "10-failure.yaml": "failed", "20-success.json": "succeeded"} {
		data, err := os.ReadFile(filepath.Join(output, name+".result.json"))
		if err != nil {
			t.Fatal(err)
		}
		var result dsc.Result
		if err := json.Unmarshal(data, &result); err != nil || result.Configuration != name || result.Outcome != outcome {
			t.Fatalf("unexpected result %s: %s (%v)", name, data, err)
		}
		if name == "05-metadata.yaml" {
			if result.InputHash != "" || result.Operation != nil || result.Error == nil || result.Error.Kind != "input" ||
				!strings.Contains(string(data), `"operation":null`) {
				t.Fatalf("invalid metadata result: %s", data)
			}
		} else if result.InputHash == "" || result.Operation == nil || *result.Operation != dsc.OperationSet {
			t.Fatalf("missing input hash or effective operation: %s", data)
		}
		if strings.Contains(string(data), "private-sidecar-value") || strings.Contains(string(data), "PRIVATE-METADATA") {
			t.Fatalf("missing input hash or leaked parameters in %s", data)
		}
		if name == "20-success.json" && result.Parameters != "20-success.parameters.yaml" {
			t.Fatalf("missing parameter identity: %+v", result)
		}
	}
	files, err := os.ReadDir(output)
	if err != nil || len(files) != 3 {
		t.Fatalf("unexpected result files (sidecar was executed?): %v (%v)", files, err)
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
		return run(ctx, args, io.Discard, io.Discard, logger, func() { ready = true; cancel() })
	}, time.Second)
	if err != nil || !ready {
		t.Fatalf("startup/shutdown: ready=%t, %v", ready, err)
	}
	if err := run(context.Background(), []string{"-interval", "0"}, io.Discard, io.Discard, logger, func() {}); err == nil {
		t.Fatal("accepted invalid startup")
	}
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"-help"}, &stdout, &stderr, logger, func() {}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help: %v", err)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "-version") {
		t.Fatalf("help stdout=%q, stderr=%q", stdout.String(), stderr.String())
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

func TestServerInitializationFailurePublished(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "inputs")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.yaml", "b.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"mode":"success","resources":[]}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	lifetime := filepath.Join(root, "lifetime")
	t.Setenv("DSCD_TEST_RESULT_PROCESS", "1")
	t.Setenv("DSCD_TEST_INIT_FAILURE", "1")
	t.Setenv("DSCD_SERVER_LIFETIME", lifetime)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client := dsc.NewClient(executable, 5*time.Second)
	writer, err := results.NewWriter(filepath.Join(root, "results"))
	if err != nil {
		t.Fatal(err)
	}
	loop := reconcile.New(dir, time.Second, func(ctx context.Context) (reconcile.DSC, error) {
		return client.Start(ctx)
	}, writer, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err := loop.Pass(context.Background()); err == nil {
		t.Fatal("initialization failure not reported")
	}
	for _, name := range []string{"a.yaml", "b.json"} {
		path, _ := writer.Destination(name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var result dsc.Result
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if result.Outcome != "failed" || result.Error == nil || result.Error.Kind != "start" {
			t.Fatalf("missing startup failure: %+v", result)
		}
	}
	data, err := os.ReadFile(lifetime)
	if err != nil || strings.Count(string(data), "start ") != 1 {
		t.Fatalf("retried startup in pass: %s %v", data, err)
	}
}
