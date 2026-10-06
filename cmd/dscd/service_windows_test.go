package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

func TestServiceControls(t *testing.T) {
	for _, control := range []svc.Cmd{svc.Stop, svc.Shutdown} {
		t.Run(string(rune(control)), func(t *testing.T) {
			h := &serviceHandler{
				logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), timeout: time.Second,
				run: func(ctx context.Context, ready func()) error { ready(); <-ctx.Done(); return nil },
			}
			requests := make(chan svc.ChangeRequest, 4)
			statuses := make(chan svc.Status, 16)
			done := make(chan uint32, 1)
			go func() { _, code := h.Execute(nil, requests, statuses); done <- code }()
			expectStatus(t, statuses, svc.StartPending)
			expectStatus(t, statuses, svc.Running)
			requests <- svc.ChangeRequest{Cmd: svc.Interrogate}
			status := expectStatus(t, statuses, svc.Running)
			if status.Accepts != svc.AcceptStop|svc.AcceptShutdown {
				t.Fatal("stop/shutdown not accepted")
			}
			requests <- svc.ChangeRequest{Cmd: control}
			expectStatus(t, statuses, svc.StopPending)
			select {
			case code := <-done:
				if code != 0 || h.err != nil {
					t.Fatalf("service exit: %d, %v", code, h.err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("service did not stop")
			}
		})
	}
}

func TestServiceStartupFailureAndStopDuringStartup(t *testing.T) {
	for _, failure := range []bool{false, true} {
		h := &serviceHandler{
			logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), timeout: time.Second,
			run: func(ctx context.Context, _ func()) error {
				if failure {
					return errors.New("startup failed")
				}
				<-ctx.Done()
				return nil
			},
		}
		requests := make(chan svc.ChangeRequest, 1)
		statuses := make(chan svc.Status, 16)
		done := make(chan uint32, 1)
		go func() { _, code := h.Execute(nil, requests, statuses); done <- code }()
		expectStatus(t, statuses, svc.StartPending)
		if !failure {
			requests <- svc.ChangeRequest{Cmd: svc.Shutdown}
			expectStatus(t, statuses, svc.StopPending)
		}
		select {
		case code := <-done:
			if (code != 0) != failure {
				t.Fatalf("exit code = %d, failure=%t", code, failure)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("startup lifecycle did not complete")
		}
	}
}

func TestServiceShutdownDeadline(t *testing.T) {
	release := make(chan struct{})
	exited := make(chan struct{})
	h := &serviceHandler{
		logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), timeout: 10 * time.Millisecond,
		run: func(ctx context.Context, ready func()) error {
			defer close(exited)
			ready()
			<-release
			return nil
		},
	}
	requests := make(chan svc.ChangeRequest, 1)
	statuses := make(chan svc.Status, 16)
	done := make(chan uint32, 1)
	go func() { _, code := h.Execute(nil, requests, statuses); done <- code }()
	expectStatus(t, statuses, svc.StartPending)
	expectStatus(t, statuses, svc.Running)
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	select {
	case code := <-done:
		close(release)
		<-exited
		if code == 0 || h.err == nil {
			t.Fatal("timed-out service reported success")
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("service shutdown was not bounded")
	}
}

func expectStatus(t *testing.T, statuses <-chan svc.Status, state svc.State) svc.Status {
	t.Helper()
	select {
	case status := <-statuses:
		if status.State != state {
			t.Fatalf("state=%v, want %v", status.State, state)
		}
		return status
	case <-time.After(3 * time.Second):
		t.Fatalf("no service status %v", state)
		return svc.Status{}
	}
}
