package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func platformRun(args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	return boundedRun(ctx, func() error {
		return run(ctx, args, os.Stdout, os.Stderr, logger, func() {})
	}, shutdownTimeout)
}
