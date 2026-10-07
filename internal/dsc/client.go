package dsc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

const (
	stdoutLimit    = 16 << 20
	pipeWait       = 2 * time.Second
	initializeWait = 10 * time.Second
)

var errServerExited = errors.New("DSC server exited unexpectedly")

// Client starts short-lived DSC server sessions using a resolved executable.
type Client struct {
	path    string
	timeout time.Duration
}

// NewClient creates a client with a positive per-configuration request timeout.
func NewClient(path string, timeout time.Duration) *Client {
	return &Client{path: path, timeout: timeout}
}

// Session owns one server. Execute and Close must be called serially.
type Session struct {
	cmd        *exec.Cmd
	parent     context.Context
	cancel     context.CancelFunc
	stdin      *os.File
	stdout     *os.File
	messages   chan response
	readDone   chan struct{}
	done       chan struct{}
	waitErr    error
	cleanupErr error
	timeout    time.Duration
	nextID     uint64
	closed     bool
}

// Start launches and initializes a server. The caller must close the returned session.
func (c *Client) Start(ctx context.Context) (*Session, error) {
	lifetime, cancel := context.WithCancel(ctx)
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create DSC stdin: %w", err)
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, errors.Join(err, inRead.Close(), inWrite.Close())
	}
	cmd := exec.CommandContext(lifetime, c.path, "server")
	cmd.Stdin, cmd.Stdout = inRead, outWrite
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, errors.Join(err, inRead.Close(), inWrite.Close(), outRead.Close(), outWrite.Close())
	}
	cmd.WaitDelay = pipeWait
	cleanup, err := startProcess(cmd)
	childEndsErr := errors.Join(inRead.Close(), outWrite.Close())
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start DSC server: %w", errors.Join(err, childEndsErr, inWrite.Close(), outRead.Close(), stderr.Close()))
	}
	s := &Session{
		cmd: cmd, parent: ctx, cancel: cancel, stdin: inWrite, stdout: outRead,
		messages: make(chan response, 1), readDone: make(chan struct{}), done: make(chan struct{}),
		timeout: c.timeout,
	}
	// No attribution is possible for shared stderr. Drain without retaining it.
	drained := make(chan struct{})
	var drainErr error
	go func() {
		defer close(drained)
		_, drainErr = io.Copy(io.Discard, stderr)
		if errors.Is(drainErr, os.ErrClosed) {
			drainErr = nil
		}
		if drainErr != nil {
			cancel()
		}
	}()
	go s.readResponses(lifetime)
	go func() {
		s.waitErr = cmd.Wait()
		s.cleanupErr = cleanup()
		<-drained // Wait closes StderrPipe, including inherited resource handles.
		s.cleanupErr = errors.Join(s.cleanupErr, drainErr)
		_ = inWrite.Close()
		_ = outRead.Close()
		cancel()
		close(s.done)
	}()
	if childEndsErr != nil {
		return nil, errors.Join(childEndsErr, s.abort())
	}
	initializing, stop := context.WithTimeout(ctx, min(c.timeout, initializeWait))
	defer stop()
	result, err := s.call(initializing, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"clientInfo":      map[string]string{"name": "dscd", "version": "dev"},
	})
	if err == nil {
		err = validateInitialization(result)
	}
	if err == nil {
		err = s.send(initializing, request{JSONRPC: "2.0", Method: "notifications/initialized"})
	}
	if err != nil {
		cleanupErr := s.abort()
		var rpcErr *rpcError
		if errors.As(err, &rpcErr) {
			// Server diagnostic text is not safe for pass-level logging.
			err = fmt.Errorf("initialize rejected with JSON-RPC code %d", rpcErr.Code)
		}
		return nil, fmt.Errorf("initialize DSC server: %w", errors.Join(err, cleanupErr))
	}
	return s, nil
}

// Execute submits captured input strings. A nonnil error means the session is
// unusable and has been terminated; normal DSC failures are only in the Result.
func (s *Session) Execute(ctx context.Context, input Input) (r Result, sessionErr error) {
	start := time.Now()
	r = newResult(input, start)
	defer func() { r.finish(start) }()
	deadline, stop := context.WithTimeout(ctx, s.timeout)
	defer stop()
	arguments := map[string]any{"operation": "set", "configuration": input.ConfigurationText}
	if input.Parameters != "" {
		arguments["parameters"] = input.ParametersText
	}
	raw, err := s.call(deadline, "tools/call", map[string]any{
		"name": "invoke_dsc_config", "arguments": arguments,
	})
	if deadline.Err() != nil {
		err = deadline.Err()
	} else if s.parent.Err() != nil {
		err = s.parent.Err()
	}
	var rpcErr *rpcError
	if errors.As(err, &rpcErr) {
		r.fail("dsc", rpcErr.Error())
		return r, nil
	}
	if err == nil {
		var diagnostic string
		var hadErrors bool
		r.DSCResult, hadErrors, diagnostic, err = parseToolResult(raw)
		if err == nil {
			switch {
			case diagnostic != "":
				r.fail("dsc", diagnostic)
			case hadErrors:
				r.fail("dsc", "DSC reported hadErrors: true")
			}
			return r, nil
		}
	}
	kind := "output"
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		kind = "canceled"
	} else if errors.Is(err, errServerExited) || errors.Is(err, io.EOF) {
		kind = "exit"
	}
	err = errors.Join(err, s.abort())
	if s.cmd.ProcessState != nil {
		if code := s.cmd.ProcessState.ExitCode(); code >= 0 {
			r.ExitCode = &code
		}
	}
	r.fail(kind, fmt.Sprintf("DSC server request failed: %v", err))
	return r, err
}

func (s *Session) readResponses(ctx context.Context) {
	defer close(s.readDone)
	reader := bufio.NewReader(s.stdout)
	for {
		line, err := readFrame(reader)
		var message response
		if err == nil {
			message, err = decodeResponse(line)
			if err == nil && message.notification {
				continue
			}
		}
		message.err = err
		select {
		case s.messages <- message:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) send(ctx context.Context, req request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.done:
		return errServerExited
	default:
	}
	written := make(chan error, 1)
	go func() {
		written <- json.NewEncoder(s.stdin).Encode(req)
	}()
	select {
	case err := <-written:
		if err != nil {
			return fmt.Errorf("write DSC request: %w", err)
		}
		return nil
	case <-ctx.Done():
		cleanupErr := s.abort()
		<-written
		return errors.Join(ctx.Err(), cleanupErr)
	case <-s.done:
		<-written
		return errServerExited
	}
}

func (s *Session) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.nextID++
	if err := s.send(ctx, request{JSONRPC: "2.0", ID: s.nextID, Method: method, Params: params}); err != nil {
		return nil, err
	}
	select {
	case message := <-s.messages:
		if message.err != nil {
			return nil, message.err
		}
		if message.id != s.nextID {
			return nil, fmt.Errorf("DSC response ID %d does not match request %d", message.id, s.nextID)
		}
		if message.rpcErr != nil {
			return nil, message.rpcErr
		}
		return message.result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, errServerExited
	}
}

func (s *Session) abort() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.cancel()
	_ = s.stdin.Close()
	_ = s.stdout.Close()
	<-s.done
	<-s.readDone
	return s.cleanupErr
}

// Close sends EOF and waits briefly for normal exit before terminating the tree.
// It is idempotent. No server is retained between passes.
func (s *Session) Close() error {
	if s.closed {
		return nil
	}
	if s.parent.Err() != nil {
		return s.abort()
	}
	err := s.stdin.Close()
	if errors.Is(err, os.ErrClosed) {
		err = nil
	}
	timer := time.NewTimer(pipeWait)
	defer timer.Stop()
	select {
	case <-s.done:
		s.closed = true
		<-s.readDone
		return errors.Join(err, s.waitErr, s.cleanupErr)
	case <-s.parent.Done():
		return errors.Join(err, s.abort())
	case <-timer.C:
		return errors.Join(err, errors.New("DSC server did not exit within shutdown grace period"), s.abort())
	}
}
