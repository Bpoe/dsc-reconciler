// Package reconcile discovers documents and serially coordinates execution and publication.
package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"time"

	"dsc-reconciler/internal/dsc"
)

// DSC applies an opaque configuration document and optional parameter file.
type DSC interface {
	Execute(context.Context, dsc.Input) dsc.Result
}

// ResultWriter validates destination names and publishes complete attempt results.
type ResultWriter interface {
	Destination(string) (string, error)
	Write(context.Context, dsc.Result) error
}

// Reconciler owns a serial periodic loop. Do not call Run or Pass concurrently.
type Reconciler struct {
	dir      string
	interval time.Duration
	dsc      DSC
	writer   ResultWriter
	log      *slog.Logger
}

// New wires one instance; interval must be positive.
func New(dir string, interval time.Duration, client DSC, writer ResultWriter, logger *slog.Logger) *Reconciler {
	return &Reconciler{dir: dir, interval: interval, dsc: client, writer: writer, log: logger}
}

// Run reconciles immediately, then on ticks until cancellation. Pass failures are retried.
func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := r.Pass(ctx); err != nil && !errors.Is(err, context.Canceled) {
			r.log.Error("reconciliation pass failed", "config_path", r.dir, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Pass discovers and attempts each eligible document once. Per-document failures are handled locally.
func (r *Reconciler) Pass(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	candidates, err := discover(r.dir)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := candidate.input.Configuration
		name := filepath.Base(path)
		target, err := r.writer.Destination(name)
		if err != nil {
			r.log.Error("unrepresentable result name; document not executed", "config_path", path, "error", err)
			continue
		}
		var result dsc.Result
		hash := ""
		err = candidate.err
		if err == nil {
			hash, err = inputHash(ctx, candidate.input)
		}
		if err != nil {
			result = dsc.InputFailure(candidate.input, err)
		} else {
			if err := ctx.Err(); err != nil {
				return err
			}
			result = r.dsc.Execute(ctx, candidate.input)
		}
		result.InputHash = hash
		level := slog.LevelInfo
		if result.Error != nil {
			level = slog.LevelError
		}
		// Do not log diagnostics or the result payload: resources may expose secrets.
		kind := ""
		message := ""
		if result.Error != nil {
			kind = result.Error.Kind
			message = result.Error.Message
		}
		r.log.Log(ctx, level, "DSC attempt completed", "config_path", path, "outcome", result.Outcome,
			"duration", time.Duration(result.DurationMS)*time.Millisecond, "exit_code", result.ExitCode,
			"parameters_path", candidate.input.Parameters, "error_kind", kind, "error", message)
		publication, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = r.writer.Write(publication, result)
		cancel()
		if err != nil {
			r.log.Error("result publication failed", "config_path", path, "result_path", target, "error", err)
		}
	}
	return nil
}
