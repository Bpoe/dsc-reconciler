// Package reconcile discovers documents and serially coordinates execution and publication.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dsc-reconciler/internal/dsc"
)

// DSC applies an opaque configuration document.
type DSC interface {
	Execute(context.Context, string) dsc.Result
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
	names, err := discover(r.dir)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(r.dir, name)
		target, err := r.writer.Destination(name)
		if err != nil {
			r.log.Error("unrepresentable result name; document not executed", "config_path", path, "error", err)
			continue
		}
		var result dsc.Result
		if err := checkInput(path); err != nil {
			result = dsc.InputFailure(path, err)
		} else {
			if err := ctx.Err(); err != nil {
				return err
			}
			result = r.dsc.Execute(ctx, path)
		}
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
			"error_kind", kind, "error", message)
		publication, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = r.writer.Write(publication, result)
		cancel()
		if err != nil {
			r.log.Error("result publication failed", "config_path", path, "result_path", target, "error", err)
		}
	}
	return nil
}

func eligibleName(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	switch filepath.Ext(name) {
	case ".yaml", ".yml", ".json":
		return true
	}
	return false
}

func discover(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("scan configuration directory %q: %w", dir, err)
	}
	var names []string
	for _, entry := range entries {
		if !eligibleName(entry.Name()) || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Mode().IsRegular() {
			// A disappeared/inaccessible candidate still needs an input-failure result.
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func checkInput(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect input %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("input %q is no longer a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open input %q: %w", path, err)
	}
	return f.Close()
}
