// dscd periodically applies local DSC documents and publishes their latest results.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"dsc-reconciler/internal/config"
	"dsc-reconciler/internal/dsc"
	"dsc-reconciler/internal/reconcile"
	"dsc-reconciler/internal/results"
)

const shutdownTimeout = 30 * time.Second

func main() {
	if err := platformRun(os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("dscd stopped with error", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer, logger *slog.Logger, ready func()) error {
	options, err := config.Parse(args, output)
	if err != nil {
		return err
	}
	if err = options.Prepare(); err != nil {
		return err
	}
	writer, err := results.NewWriter(options.ResultsDir)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	client := dsc.NewClient(options.DSCPath, options.ExecutionTimeout)
	loop := reconcile.New(options.ConfigDir, options.Interval, client, writer, logger)
	logger.Info("dscd started", "config_path", options.ConfigDir, "result_path", options.ResultsDir,
		"interval", options.Interval, "execution_timeout", options.ExecutionTimeout)
	ready()
	loop.Run(ctx)
	logger.Info("dscd stopped")
	return nil
}

// The command boundary, not a detached writer goroutine, bounds blocked filesystem cleanup.
func boundedRun(ctx context.Context, worker func() error, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- worker() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("shutdown exceeded %s; exiting with cleanup incomplete", timeout)
	}
}
