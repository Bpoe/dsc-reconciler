package dsc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"
	"unicode/utf8"
)

const (
	stdoutLimit = 16 << 20
	stderrLimit = 1 << 20
	pipeWait    = 2 * time.Second
)

// Client invokes a resolved executable with a bounded execution time and output.
type Client struct {
	path    string
	timeout time.Duration
}

// NewClient creates a client. The caller supplies a resolved path and positive timeout.
func NewClient(path string, timeout time.Duration) *Client {
	return &Client{path: path, timeout: timeout}
}

// Execute applies one document. Execution failures are carried in the result.
func (c *Client) Execute(ctx context.Context, path string) (r Result) {
	start := time.Now()
	r = newResult(path, start)
	defer func() { r.finish(start) }()
	deadline, stop := context.WithTimeout(ctx, c.timeout)
	defer stop()
	execution, cancel := context.WithCancel(deadline)
	defer cancel()
	out := limitedBuffer{limit: stdoutLimit, cancel: cancel}
	diagnostics := limitedBuffer{limit: stderrLimit, cancel: cancel}
	cmd := exec.CommandContext(execution, c.path, "config", "set", "--file", path, "--output-format", "json")
	cmd.Stdout, cmd.Stderr = &out, &diagnostics
	cmd.WaitDelay = pipeWait
	cleanup, startErr := startProcess(cmd)
	var waitErr, cleanupErr error
	if startErr == nil {
		waitErr = cmd.Wait()
		cleanupErr = cleanup()
		if code := cmd.ProcessState.ExitCode(); code != -1 {
			r.ExitCode = &code
		}
	}
	r.Stderr = diagnostics.buffer.String()
	if diagnostics.exceeded {
		r.Stderr += "\n[dscd: stderr capture limit exceeded]"
	}
	var payload json.RawMessage
	var hadErrors bool
	var outputErr error
	if !out.exceeded {
		payload, hadErrors, outputErr = parseOutput(out.buffer.Bytes())
	}
	if outputErr == nil {
		r.DSCResult = payload
	}
	switch {
	case deadline.Err() != nil:
		r.fail("canceled", fmt.Sprintf("DSC execution canceled: %v", deadline.Err()))
	case startErr != nil:
		r.fail("start", fmt.Sprintf("start DSC: %v", startErr))
	case out.exceeded || diagnostics.exceeded:
		r.fail("output", "DSC output capture limit exceeded; process tree terminated")
	case r.ExitCode == nil || *r.ExitCode != 0:
		r.fail("exit", fmt.Sprintf("DSC exited unsuccessfully: %v", waitErr))
	case waitErr != nil || cleanupErr != nil:
		r.fail("output", fmt.Sprintf("collect DSC output or clean up processes: %v", errors.Join(waitErr, cleanupErr)))
	case outputErr != nil:
		r.fail("output", outputErr.Error())
	case hadErrors:
		r.fail("dsc", "DSC reported hadErrors: true")
	}
	if cleanupErr != nil && r.Error != nil {
		r.Error.Message += fmt.Sprintf("; process cleanup: %v", cleanupErr)
	}
	return r
}

// Each buffer has a single os/exec copy goroutine; only inspect it after Wait.
type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.buffer.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.exceeded = true
		b.cancel()
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func parseOutput(data []byte) (json.RawMessage, bool, error) {
	if !utf8.Valid(data) {
		return nil, false, errors.New("DSC stdout is not valid UTF-8")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, false, errors.New("DSC stdout must contain exactly one JSON object")
	}
	for name, opening := range map[string]byte{"metadata": '{', "results": '[', "messages": '['} {
		value := bytes.TrimSpace(object[name])
		if len(value) == 0 || value[0] != opening {
			return nil, false, fmt.Errorf("DSC stdout has missing or invalid %s", name)
		}
	}
	var hadErrors bool
	raw := bytes.TrimSpace(object["hadErrors"])
	if !bytes.Equal(raw, []byte("true")) && !bytes.Equal(raw, []byte("false")) {
		return nil, false, errors.New("DSC stdout has missing or invalid hadErrors")
	}
	if err := json.Unmarshal(raw, &hadErrors); err != nil {
		return nil, false, fmt.Errorf("decode DSC hadErrors: %w", err)
	}
	return json.RawMessage(bytes.Clone(data)), hadErrors, nil
}
