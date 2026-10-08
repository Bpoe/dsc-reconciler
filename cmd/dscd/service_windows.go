package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

func platformRun(args []string) error {
	service, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("detect SCM service: %w", err)
	}
	if !service {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		return boundedRun(ctx, func() error {
			return run(ctx, args, os.Stdout, os.Stderr, logger, func() {})
		}, shutdownTimeout)
	}
	log, err := eventlog.Open("dscd")
	if err != nil {
		return fmt.Errorf("open dscd event log: %w", err)
	}
	defer log.Close()
	output := eventWriter{log: log}
	logger := slog.New(slog.NewJSONHandler(output, nil))
	handler := &serviceHandler{
		logger:  logger,
		timeout: shutdownTimeout,
		run: func(ctx context.Context, ready func()) error {
			return run(ctx, args, output, output, logger, ready)
		},
	}
	if err := svc.Run("dscd", handler); err != nil {
		logger.Error("SCM dispatcher failed", "error", err)
		return err
	}
	return handler.err
}

type eventWriter struct{ log *eventlog.Log }

func (w eventWriter) Write(p []byte) (int, error) {
	if err := w.log.Info(1, string(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

type serviceHandler struct {
	run     func(context.Context, func()) error
	logger  *slog.Logger
	timeout time.Duration
	err     error
}

func (h *serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	status := svc.Status{State: svc.StartPending, WaitHint: 30000, CheckPoint: 1}
	statuses <- status
	go func() { done <- h.run(ctx, func() { close(ready) }) }()
	ticks := time.NewTicker(time.Second)
	defer ticks.Stop()
	var deadline <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	stop := func() {
		if status.State == svc.StopPending {
			return
		}
		cancel()
		status = svc.Status{State: svc.StopPending, WaitHint: uint32(h.timeout.Milliseconds()), CheckPoint: 1}
		statuses <- status
		timer = time.NewTimer(h.timeout)
		deadline = timer.C
	}
	for {
		select {
		case <-ready:
			ready = nil
			if status.State != svc.StopPending {
				status = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
				statuses <- status
			}
		case request, ok := <-requests:
			if !ok {
				requests = nil
				stop()
				continue
			}
			switch request.Cmd {
			case svc.Stop, svc.Shutdown:
				stop()
			case svc.Interrogate:
				statuses <- status
			}
		case <-ticks.C:
			if status.State == svc.StartPending || status.State == svc.StopPending {
				status.CheckPoint++
				statuses <- status
			}
		case err := <-done:
			h.err = err
			if err != nil {
				h.logger.Error("service stopped with error", "error", err)
				return true, 1
			}
			return false, 0
		case <-deadline:
			h.err = fmt.Errorf("service shutdown exceeded %s; exiting with cleanup incomplete", h.timeout)
			h.logger.Error("service shutdown deadline exceeded", "error", h.err)
			return true, 1
		}
	}
}
