package dsc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const validOutput = `{"metadata":{},"results":[],"messages":[],"hadErrors":false,"future":{"integer":9007199254740993}}`
const initializedResult = `{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1"}}`

func TestMain(m *testing.M) {
	switch os.Getenv("DSCD_TEST_HELPER") {
	case "server":
		os.Exit(helperServer())
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

func helperServer() int {
	if len(os.Args) != 2 || os.Args[1] != "server" {
		return 91
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 100<<20)
	encode := json.NewEncoder(os.Stdout)
	if !scanner.Scan() {
		return 92
	}
	var init struct {
		JSONRPC string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			ProtocolVersion string                         `json:"protocolVersion"`
			Capabilities    map[string]json.RawMessage     `json:"capabilities"`
			ClientInfo      struct{ Name, Version string } `json:"clientInfo"`
		} `json:"params"`
	}
	if json.Unmarshal(scanner.Bytes(), &init) != nil || init.JSONRPC != "2.0" || init.ID != 1 ||
		init.Method != "initialize" || init.Params.ProtocolVersion != protocolVersion ||
		string(init.Params.Capabilities["tools"]) != "{}" || init.Params.ClientInfo.Name != "dscd" || init.Params.ClientInfo.Version == "" {
		return 93
	}
	switch os.Getenv("DSCD_TEST_INIT") {
	case "hang":
		for {
			time.Sleep(time.Hour)
		}
	case "malformed":
		fmt.Println("bad JSON")
		return 0
	case "error":
		_ = encode.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32602, "message": "private init diagnostic"}})
		for scanner.Scan() {
		}
		return 0
	case "shape":
		_ = encode.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{}})
	default:
		_ = encode.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": json.RawMessage(initializedResult)})
	}
	if !scanner.Scan() {
		return 94
	}
	var notification map[string]json.RawMessage
	if json.Unmarshal(scanner.Bytes(), &notification) != nil || string(notification["method"]) != `"notifications/initialized"` || notification["id"] != nil {
		return 95
	}
	if os.Getenv("DSCD_TEST_NO_READ") == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}
	lastID := uint64(1)
	for scanner.Scan() {
		var req struct {
			JSONRPC string `json:"jsonrpc"`
			ID      uint64 `json:"id"`
			Method  string `json:"method"`
			Params  struct {
				Name      string `json:"name"`
				Arguments struct {
					Operation     string  `json:"operation"`
					Configuration string  `json:"configuration"`
					Parameters    *string `json:"parameters"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil || req.JSONRPC != "2.0" || req.ID != lastID+1 ||
			req.Method != "tools/call" || req.Params.Name != "invoke_dsc_config" || req.Params.Arguments.Operation != "set" {
			return 96
		}
		lastID = req.ID
		mode := req.Params.Arguments.Configuration
		payload := json.RawMessage(validOutput)
		switch mode {
		case "rpc-error":
			_ = encode.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "private operation diagnostic"}})
			continue
		case "dsc-error":
			payload = json.RawMessage(strings.Replace(validOutput, `"hadErrors":false`, `"hadErrors":true`, 1))
		case "tool-error":
			_ = encode.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"isError": true, "content": []any{map[string]string{"type": "text", "text": "private tool diagnostic"}}}})
			continue
		case "mismatch":
			req.ID++
		case "malformed":
			fmt.Println("bad JSON")
			continue
		case "structured":
			_ = encode.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"structuredContent": map[string]any{"result": 42}}})
			continue
		case "exit":
			return 7
		case "partial":
			fmt.Print(`{"jsonrpc":`)
			return 7
		case "hang":
			for {
				time.Sleep(time.Hour)
			}
		case "notifications":
			_ = encode.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"unknown": true}})
		case "stdout-limit":
			fmt.Println(strings.Repeat("x", stdoutLimit+1))
			continue
		case "stderr-flood":
			fmt.Fprint(os.Stderr, strings.Repeat("private stderr", 200000))
		case "descendant", "descendant-hang", "descendant-exit":
			path := os.Getenv("DSCD_CHILD_READY")
			executable, _ := os.Executable()
			child := exec.Command(executable)
			child.Env = append(os.Environ(), "DSCD_TEST_HELPER=child")
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			if child.Start() != nil {
				return 97
			}
			if os.WriteFile(path+".pid", []byte(strconv.Itoa(child.Process.Pid)), 0600) != nil {
				return 98
			}
			if mode == "descendant-exit" {
				return 7
			}
			if mode == "descendant-hang" {
				for {
					time.Sleep(time.Hour)
				}
			}
		case "inline":
			if req.Params.Arguments.Parameters == nil || *req.Params.Arguments.Parameters != "message: exact text\n" {
				return 99
			}
		default:
			if mode != "empty-sidecar" && req.Params.Arguments.Parameters != nil {
				return 100
			}
			if mode == "empty-sidecar" && (req.Params.Arguments.Parameters == nil || *req.Params.Arguments.Parameters != "") {
				return 101
			}
		}
		_ = encode.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"structuredContent": map[string]any{"result": payload}, "future": true}})
	}
	if os.Getenv("DSCD_TEST_EOF_HANG") == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}
	return 0
}

func helperClient(t *testing.T) (*Client, string) {
	t.Helper()
	t.Setenv("DSCD_TEST_HELPER", "server")
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
	path := filepath.Join(dir, "fake dsc"+filepath.Ext(executable))
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	return NewClient(path, 10*time.Second), dir
}

func startSession(t *testing.T, client *Client, ctx context.Context) *Session {
	t.Helper()
	session, err := client.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	return session
}

func TestSessionNormalFailuresAndInlineInputs(t *testing.T) {
	client, _ := helperClient(t)
	session := startSession(t, client, context.Background())
	pid := session.cmd.Process.Pid
	for _, test := range []struct {
		mode, kind string
		parameters bool
	}{
		{"success", "", false}, {"inline", "", true}, {"empty-sidecar", "", true},
		{"notifications", "", false}, {"rpc-error", "dsc", false}, {"dsc-error", "dsc", false},
		{"tool-error", "dsc", false}, {"stderr-flood", "", false}, {"success", "", false},
	} {
		in := Input{Configuration: "a.yaml", ConfigurationText: test.mode}
		if test.parameters {
			in.Parameters = "a.parameters.json"
			if test.mode == "inline" {
				in.ParametersText = "message: exact text\n"
			}
		}
		r, err := session.Execute(context.Background(), in)
		if err != nil {
			t.Fatalf("%s: %v", test.mode, err)
		}
		if (r.Error == nil) != (test.kind == "") || r.Error != nil && r.Error.Kind != test.kind {
			t.Fatalf("%s: %+v", test.mode, r)
		}
		if r.ExitCode != nil || r.Stderr != "" || session.cmd.Process.Pid != pid {
			t.Fatalf("invented per-attempt process diagnostics or replaced session: %+v", r)
		}
		if r.StartedAt.IsZero() || r.FinishedAt.IsZero() || r.DurationMS < 0 {
			t.Fatal("missing timing")
		}
		if test.kind == "" && string(r.DSCResult) != validOutput {
			t.Fatal("lost exact nested DSC result")
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if session.cmd.ProcessState.ExitCode() != 0 {
		t.Fatal("server did not exit normally on EOF")
	}
	if !processStopped(pid) {
		t.Fatal("server remained alive after Close")
	}
}

func TestProtocolFailuresTerminateSession(t *testing.T) {
	client, _ := helperClient(t)
	for _, mode := range []string{"mismatch", "malformed", "structured", "exit", "partial", "stdout-limit"} {
		t.Run(mode, func(t *testing.T) {
			s := startSession(t, client, context.Background())
			r, err := s.Execute(context.Background(), Input{Configuration: "a.yaml", ConfigurationText: mode})
			if err == nil || r.Error == nil || r.Outcome != "failed" || !s.closed {
				t.Fatalf("protocol failure was not fatal to session: %+v %v", r, err)
			}
			if !processStopped(s.cmd.Process.Pid) {
				t.Fatal("server still running")
			}
		})
	}
}

func TestRequestTimeoutAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		client, _ := helperClient(t)
		ctx, cancel := context.WithCancel(context.Background())
		s := startSession(t, client, ctx)
		s.timeout = 100 * time.Millisecond
		if canceled {
			cancel()
		}
		r, err := s.Execute(ctx, Input{Configuration: "a.yaml", ConfigurationText: "hang"})
		cancel()
		if err == nil || r.Outcome != "canceled" || r.Error.Kind != "canceled" || !s.closed {
			t.Fatalf("timeout/cancellation: %+v %v", r, err)
		}
		if !processStopped(s.cmd.Process.Pid) {
			t.Fatal("canceled server survived")
		}
	}
}

func TestInitializationFailures(t *testing.T) {
	client, _ := helperClient(t)
	for _, mode := range []string{"error", "shape", "malformed", "hang"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("DSCD_TEST_INIT", mode)
			client.timeout = 500 * time.Millisecond
			s, err := client.Start(context.Background())
			if err == nil || s != nil {
				t.Fatal("accepted initialization failure")
			}
			if strings.Contains(err.Error(), "private init") {
				t.Fatal("unsafe startup diagnostics")
			}
		})
	}
	missing := NewClient(filepath.Join(t.TempDir(), "missing"), time.Second)
	if _, err := missing.Start(context.Background()); err == nil {
		t.Fatal("started missing executable")
	}
}

func TestCloseForcesUnresponsiveServer(t *testing.T) {
	client, _ := helperClient(t)
	t.Setenv("DSCD_TEST_EOF_HANG", "1")
	s, err := client.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := s.Close(); err == nil {
		t.Fatal("forced close not reported")
	}
	if time.Since(start) > 8*time.Second || !processStopped(s.cmd.Process.Pid) {
		t.Fatal("close was not bounded")
	}
}

func TestDescendantCleanup(t *testing.T) {
	for _, mode := range []string{"descendant", "descendant-hang", "descendant-exit"} {
		t.Run(mode, func(t *testing.T) {
			client, dir := helperClient(t)
			ready := filepath.Join(dir, "ready")
			t.Setenv("DSCD_CHILD_READY", ready)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := startSession(t, client, ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = s.Execute(ctx, Input{Configuration: "a.yaml", ConfigurationText: mode})
			}()
			if mode != "descendant-exit" {
				waitFor(t, func() bool { _, err := os.Stat(ready); return err == nil })
			}
			var pid int
			waitFor(t, func() bool {
				raw, err := os.ReadFile(ready + ".pid")
				if err != nil {
					return false
				}
				pid, err = strconv.Atoi(string(raw))
				return err == nil && pid > 0
			})
			if mode == "descendant-hang" {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				t.Fatal("execution stuck")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
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

func TestProtocolValidation(t *testing.T) {
	for _, line := range []string{
		`null`, `[]`, `garbage`, `{"jsonrpc":"1.0","id":2,"result":{}}`,
		`{"jsonrpc":"2.0","id":null,"result":{}}`, `{"jsonrpc":"2.0","id":"2","result":{}}`,
		`{"jsonrpc":"2.0","id":2,"result":{},"error":{}}`, `{"jsonrpc":"2.0","id":2}`,
		`{"jsonrpc":"2.0","id":2,"result":[]}`, `{"jsonrpc":"2.0","id":2,"method":"request"}`,
		`{"jsonrpc":"2.0","method":"notification","params":false}`,
		`{"jsonrpc":"2.0","id":2,"method":null,"result":{}}`,
		`{"jsonrpc":"2.0","id":2,"error":{"code":"bad","message":"bad"}}`,
	} {
		if _, err := decodeResponse([]byte(line)); err == nil {
			t.Errorf("accepted %s", line)
		}
	}
	for _, data := range []string{"", "null", "[]", "{}", validOutput + "{}", strings.Replace(validOutput, `"hadErrors":false`, `"hadErrors":null`, 1)} {
		if _, _, err := parseOutput([]byte(data)); err == nil {
			t.Errorf("accepted DSC result %q", data)
		}
	}
	r := InputFailure(Input{Configuration: "a.yaml"}, errors.New("unreadable"))
	data, _ := json.Marshal(r)
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 10 || string(fields["exitCode"]) != "null" {
		t.Fatal("changed envelope")
	}
}

func TestBlockedRequestWriteIsCanceled(t *testing.T) {
	client, _ := helperClient(t)
	t.Setenv("DSCD_TEST_NO_READ", "1")
	s := startSession(t, client, context.Background())
	s.timeout = 100 * time.Millisecond
	r, err := s.Execute(context.Background(), Input{Configuration: "a.yaml", ConfigurationText: strings.Repeat("x", 2<<20)})
	if err == nil || r.Outcome != "canceled" || !s.closed || !processStopped(s.cmd.Process.Pid) {
		t.Fatalf("blocked write wasn't canceled: %+v %v", r, err)
	}
}

func TestCancelIdleSession(t *testing.T) {
	client, _ := helperClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	s := startSession(t, client, ctx)
	cancel()
	select {
	case <-s.done:
	case <-time.After(8 * time.Second):
		t.Fatal("idle server survived cancellation")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !processStopped(s.cmd.Process.Pid) {
		t.Fatal("idle server remained alive")
	}
}

func TestFrameLimitsAndToolShapes(t *testing.T) {
	for _, size := range []int{stdoutLimit, stdoutLimit + 1} {
		frame := strings.Repeat("x", size-1) + "\n"
		got, err := readFrame(bufio.NewReader(strings.NewReader(frame)))
		if size == stdoutLimit && (err != nil || len(got) != size) {
			t.Fatalf("boundary frame rejected: %v", err)
		}
		if size > stdoutLimit && err == nil {
			t.Fatal("oversized frame accepted")
		}
	}
	for _, raw := range []string{`{}`, `{"structuredContent":null}`, `{"structuredContent":[]}`, `{"isError":null}`, `{"isError":"false"}`} {
		if _, _, _, err := parseToolResult(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted malformed tool result %s", raw)
		}
	}
}
