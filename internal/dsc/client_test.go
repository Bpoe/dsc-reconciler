package dsc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Synthetic fixture following the official v3.1.0 config/set result reference.
const validOutput = `{"metadata":{"Microsoft.DSC":{"version":"3.1.0","operation":"Set","executionType":"Actual"}},"results":[],"messages":[],"hadErrors":false,"future":{"integer":9007199254740993}}`

func TestMain(m *testing.M) {
	switch os.Getenv("DSCD_TEST_HELPER") {
	case "dsc":
		os.Exit(helperDSC())
	case "child":
		if err := os.WriteFile(os.Getenv("DSCD_CHILD_READY"), []byte("ready"), 0600); err != nil {
			os.Exit(90)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	os.Exit(m.Run())
}

func helperDSC() int {
	if len(os.Args) != 7 || !slices.Equal(os.Args[1:4], []string{"config", "set", "--file"}) ||
		!slices.Equal(os.Args[5:], []string{"--output-format", "json"}) || !filepath.IsAbs(os.Args[4]) {
		return 91
	}
	path := os.Args[4]
	data, err := os.ReadFile(path)
	if err != nil {
		return 92
	}
	mode := string(data)
	switch mode {
	case "invalid":
		fmt.Print("not JSON")
	case "dsc-error":
		fmt.Print(strings.Replace(validOutput, `"hadErrors":false`, `"hadErrors":true`, 1))
	case "exit":
		fmt.Print(validOutput)
		fmt.Fprint(os.Stderr, "private diagnostic")
		return 7
	case "invalid-exit":
		fmt.Print("bad JSON")
		return 8
	case "stdout-limit":
		fmt.Print(strings.Repeat("x", stdoutLimit+1))
	case "stderr-limit":
		fmt.Print(validOutput)
		fmt.Fprint(os.Stderr, strings.Repeat("x", stderrLimit+1))
	case "hang":
		for {
			time.Sleep(time.Hour)
		}
	case "descendant", "descendant-exit":
		executable, err := os.Executable()
		if err != nil {
			return 93
		}
		child := exec.Command(executable)
		child.Env = append(os.Environ(), "DSCD_TEST_HELPER=child", "DSCD_CHILD_READY="+path+".ready")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			return 94
		}
		if err := os.WriteFile(path+".pid", []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
			_ = child.Process.Kill()
			return 95
		}
		if mode == "descendant" {
			for {
				time.Sleep(time.Hour)
			}
		}
		fmt.Print(validOutput)
	default:
		fmt.Print(validOutput)
		fmt.Fprint(os.Stderr, "private diagnostic")
	}
	return 0
}

func helperClient(t *testing.T) (*Client, string) {
	t.Helper()
	t.Setenv("DSCD_TEST_HELPER", "dsc")
	dir := filepath.Join(t.TempDir(), "directory with spaces")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(dir, "fake dsc"+filepath.Ext(executable))
	if err := os.WriteFile(copyPath, data, 0700); err != nil {
		t.Fatal(err)
	}
	return NewClient(copyPath, 10*time.Second), dir
}

func writeMode(t *testing.T, dir, mode string) string {
	t.Helper()
	path := filepath.Join(dir, mode+" config.yaml")
	if err := os.WriteFile(path, []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecuteClassification(t *testing.T) {
	client, dir := helperClient(t)
	for _, test := range []struct {
		mode, kind string
		payload    bool
		code       int
	}{
		{"success", "", true, 0},
		{"invalid", "output", false, 0},
		{"dsc-error", "dsc", true, 0},
		{"exit", "exit", true, 7},
		{"invalid-exit", "exit", false, 8},
		{"stdout-limit", "output", false, -1},
		{"stderr-limit", "output", true, -1},
	} {
		t.Run(test.mode, func(t *testing.T) {
			path := writeMode(t, dir, test.mode)
			result := client.Execute(context.Background(), path)
			kind := ""
			if result.Error != nil {
				kind = result.Error.Kind
			}
			if kind != test.kind || (result.DSCResult != nil) != test.payload {
				t.Fatalf("kind=%q payload=%t outcome=%s error=%+v", kind, result.DSCResult != nil, result.Outcome, result.Error)
			}
			if test.code >= 0 && (result.ExitCode == nil || *result.ExitCode != test.code) {
				t.Errorf("exit code = %v, want %d", result.ExitCode, test.code)
			}
			if result.Configuration != filepath.Base(path) || result.SchemaVersion != 1 ||
				result.StartedAt.IsZero() || result.FinishedAt.IsZero() || result.DurationMS < 0 {
				t.Fatal("invalid result identity/times")
			}
			if test.mode == "success" && (result.Outcome != "succeeded" || result.Stderr != "private diagnostic") {
				t.Fatalf("success = %+v", result)
			}
			if len(result.Stderr) > stderrLimit+100 {
				t.Fatal("unbounded stderr")
			}
		})
	}
}

func TestStartAndCancellation(t *testing.T) {
	client, dir := helperClient(t)
	path := writeMode(t, dir, "hang")
	missing := NewClient(filepath.Join(dir, "missing"), time.Second)
	if r := missing.Execute(context.Background(), path); r.Error.Kind != "start" || r.ExitCode != nil {
		t.Fatalf("start failure = %+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := client.Execute(ctx, path); r.Outcome != "canceled" || r.Error.Kind != "canceled" || r.ExitCode != nil {
		t.Fatalf("pre-canceled = %+v", r)
	}
	client.timeout = 100 * time.Millisecond
	if r := client.Execute(context.Background(), path); r.Outcome != "canceled" || r.DurationMS > 5000 {
		t.Fatalf("timeout = %+v", r)
	}
}

func TestDescendantCleanup(t *testing.T) {
	for _, mode := range []string{"descendant", "descendant-exit"} {
		t.Run(mode, func(t *testing.T) {
			client, dir := helperClient(t)
			path := writeMode(t, dir, mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan Result, 1)
			go func() { done <- client.Execute(ctx, path) }()
			waitFor(t, func() bool { _, err := os.Stat(path + ".ready"); return err == nil })
			pidBytes, err := os.ReadFile(path + ".pid")
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(pidBytes))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "descendant" {
				cancel()
			}
			select {
			case result := <-done:
				if mode == "descendant" && result.Outcome != "canceled" {
					t.Fatalf("cancel = %+v", result)
				}
				if mode == "descendant-exit" && (result.Error == nil || result.Error.Kind != "output") {
					t.Fatalf("inherited pipe = %+v", result)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("execution did not finish within cleanup bound")
			}
			waitFor(t, func() bool { return processStopped(pid) })
		})
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestParseOutput(t *testing.T) {
	for _, data := range []string{
		"", "null", "[]", "{}", validOutput + "{}",
		strings.Replace(validOutput, "Actual", string([]byte{0xff}), 1),
		strings.Replace(validOutput, `"metadata":{`, `"metadata":null,"unused":{`, 1),
		strings.Replace(validOutput, `"results":[]`, `"results":null`, 1),
		strings.Replace(validOutput, `"messages":[]`, `"messages":{}`, 1),
		strings.Replace(validOutput, `"hadErrors":false`, `"hadErrors":null`, 1),
		strings.Replace(validOutput, `"hadErrors":false`, `"hadErrors":"false"`, 1),
	} {
		if _, _, err := parseOutput([]byte(data)); err == nil {
			t.Errorf("accepted invalid output %q", data)
		}
	}
	payload, hadErrors, err := parseOutput([]byte(validOutput))
	if err != nil || hadErrors || string(payload) != validOutput {
		t.Fatalf("lost payload: %s, %t, %v", payload, hadErrors, err)
	}
	result := InputFailure("a.yaml", fmt.Errorf("unreadable"))
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 10 || string(fields["exitCode"]) != "null" || string(fields["dscResult"]) != "null" {
		t.Fatalf("envelope fields = %s (%v)", data, err)
	}
}

func TestCaptureLimitThroughCopy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &limitedBuffer{limit: 10, cancel: cancel}
	// Hiding WriterTo exercises io.Copy's ReaderFrom optimization if one is exposed.
	source := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 100))}
	n, err := io.Copy(b, source)
	if err != nil || n != 100 || b.buffer.Len() != 10 || !b.exceeded || ctx.Err() == nil {
		t.Fatalf("capture: n=%d len=%d exceeded=%t canceled=%v err=%v", n, b.buffer.Len(), b.exceeded, ctx.Err(), err)
	}
}
