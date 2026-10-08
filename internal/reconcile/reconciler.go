// Package reconcile discovers documents and serially coordinates execution and publication.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
)

// DSC is a pass-scoped server session. Execute returns an error only when the
// session is no longer usable; every attempted input still receives a Result.
type DSC interface {
	Execute(context.Context, dsc.Input) (dsc.Result, error)
	Close() error
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
	start    func(context.Context) (DSC, error)
	writer   ResultWriter
	log      *slog.Logger
}

// New wires one instance; interval must be positive.
func New(dir string, interval time.Duration, start func(context.Context) (DSC, error), writer ResultWriter, logger *slog.Logger) *Reconciler {
	return &Reconciler{dir: dir, interval: interval, start: start, writer: writer, log: logger}
}

// Run reconciles immediately, then waits the interval after each pass, including failures.
func (r *Reconciler) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := r.Pass(ctx); err != nil && !errors.Is(err, context.Canceled) {
			r.log.Error("reconciliation pass failed", "config_path", r.dir, "error", err)
		}
		timer := time.NewTimer(r.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Pass discovers and attempts each eligible document once. Per-document failures are handled locally.
func (r *Reconciler) Pass(ctx context.Context) (passErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	candidates, err := discover(r.dir)
	if err != nil {
		return err
	}
	var server DSC
	var startErr error
	defer func() {
		if server != nil {
			passErr = errors.Join(passErr, server.Close())
		}
	}()
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
		input := candidate.input
		if err == nil && candidate.metadataPath != "" {
			input.Operation, err = readMetadata(ctx, candidate.metadataPath)
		}
		if err == nil {
			input, err = readInput(ctx, input)
			if err == nil {
				hash = inputHash(input)
			}
		}
		if err != nil {
			result = dsc.InputFailure(input, err)
		} else {
			if err := ctx.Err(); err != nil {
				return err
			}
			if server == nil && startErr == nil {
				server, startErr = r.start(ctx)
				if startErr != nil {
					// A factory may return a typed nil session along with its error.
					server = nil
				}
			}
			if startErr != nil {
				result = dsc.StartFailure(input, startErr)
			} else {
				result, err = server.Execute(ctx, input)
				if err != nil {
					// The failed input is never retried; recovery is for the next document.
					if closeErr := server.Close(); closeErr != nil {
						passErr = errors.Join(passErr, fmt.Errorf("close unusable DSC session: %w", closeErr))
					}
					server = nil
				}
			}
		}
		result.InputHash = hash
		level := slog.LevelInfo
		if result.Error != nil {
			level = slog.LevelError
		}
		// Do not log diagnostics or the result payload: resources may expose secrets.
		kind := ""
		if result.Error != nil {
			kind = result.Error.Kind
		}
		r.log.Log(ctx, level, "DSC attempt completed", "config_path", path, "outcome", result.Outcome,
			"duration", time.Duration(result.DurationMS)*time.Millisecond, "exit_code", result.ExitCode,
			"parameters_path", input.Parameters, "operation", result.Operation, "error_kind", kind)
		publication, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = r.writer.Write(publication, result)
		cancel()
		if err != nil {
			r.log.Error("result publication failed", "config_path", path, "result_path", target, "error", err)
		}
	}
	if startErr != nil {
		return errors.Join(passErr, fmt.Errorf("DSC server unavailable for remaining configurations in pass: %w", startErr))
	}
	return passErr
}
