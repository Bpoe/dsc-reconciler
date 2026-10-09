// Package reconcile discovers documents and serially coordinates execution and publication.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
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

// Reconciler owns serial periodic and filesystem-triggered work.
// Do not call Run or Pass concurrently.
type Reconciler struct {
	dir      string
	interval time.Duration
	start    func(context.Context) (DSC, error)
	writer   ResultWriter
	log      *slog.Logger
	watch    func() (eventSource, error)
}

// New wires one instance; interval must be positive.
func New(dir string, interval time.Duration, start func(context.Context) (DSC, error), writer ResultWriter, logger *slog.Logger) *Reconciler {
	return &Reconciler{dir: dir, interval: interval, start: start, writer: writer, log: logger, watch: newEventSource}
}

// Run performs full passes at the configured interval and targeted work between
// them. Targeted attempts never reset the full-pass timer or overlap other work.
func (r *Reconciler) Run(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	pending := newPendingChanges()
	var watcher *directoryWatch
	closeWatch := func() {
		if watcher != nil {
			if err := watcher.close(); err != nil {
				r.log.Error("close filesystem watcher failed", "config_path", r.dir, "error", err)
			}
			watcher = nil
		}
	}
	defer closeWatch()
	watchFailure := func(err error) {
		r.log.Error("filesystem watching unavailable; periodic reconciliation continues", "config_path", r.dir, "error", err)
		closeWatch()
	}
	fullPass := func() {
		if watcher != nil {
			if err := watcher.validate(r.dir); err != nil {
				watchFailure(err)
			}
		}
		if watcher == nil {
			var err error
			watcher, err = startDirectoryWatch(ctx, r.dir, r.watch, pending)
			if err != nil {
				watchFailure(err)
			}
		}
		if err := r.Pass(ctx); err != nil && !errors.Is(err, context.Canceled) {
			r.log.Error("reconciliation pass failed", "config_path", r.dir, "error", err)
		}
	}
	fullPass()
	nextFull := time.Now().Add(r.interval)
	timer := time.NewTimer(r.interval)
	defer timer.Stop()
	var batch map[string]map[string]struct{}
	var bases []string
	for {
		if ctx.Err() != nil {
			return
		}
		if watcher != nil {
			if err := watcher.failure(); err != nil {
				watchFailure(err)
			}
		}
		if !time.Now().Before(nextFull) {
			fullPass()
			nextFull = time.Now().Add(r.interval)
			timer.Reset(r.interval)
			continue
		}
		if len(bases) == 0 {
			batch = pending.take()
			for base := range batch {
				bases = append(bases, base)
			}
			sort.Strings(bases)
		}
		if len(bases) != 0 {
			base := bases[0]
			bases = bases[1:]
			// Merge notifications received while this key waited in the batch.
			// Anything received after dispatch remains pending for a follow-up.
			for name := range pending.takeBase(base) {
				batch[base][name] = struct{}{}
			}
			if err := r.changed(ctx, batch[base]); err != nil && !errors.Is(err, context.Canceled) {
				r.log.Error("targeted reconciliation failed", "config_path", r.dir, "error", err)
			}
			delete(batch, base)
			continue
		}
		var watchDone <-chan struct{}
		if watcher != nil {
			watchDone = watcher.done
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-pending.wake:
		case <-watchDone:
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
	return r.execute(ctx, candidates)
}

func (r *Reconciler) changed(ctx context.Context, names map[string]struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	candidates, err := changedCandidates(r.dir, names)
	if err != nil {
		return err
	}
	return r.execute(ctx, candidates)
}

// execution owns a session for a full pass or one targeted dispatch.
type execution struct {
	r        *Reconciler
	server   DSC
	startErr error
	closeErr error
}

func (r *Reconciler) execute(ctx context.Context, candidates []candidate) (passErr error) {
	pass := execution{r: r}
	defer func() {
		if pass.server != nil {
			pass.closeErr = errors.Join(pass.closeErr, pass.server.Close())
		}
		passErr = errors.Join(passErr, pass.closeErr)
	}()
	for _, candidate := range candidates {
		if err := pass.attempt(ctx, candidate); err != nil {
			return err
		}
	}
	if pass.startErr != nil {
		return fmt.Errorf("DSC server unavailable for remaining configurations in pass: %w", pass.startErr)
	}
	return nil
}

func (p *execution) attempt(ctx context.Context, candidate candidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r := p.r
	path := candidate.input.Configuration
	name := filepath.Base(path)
	target, err := r.writer.Destination(name)
	if err != nil {
		r.log.Error("unrepresentable result name; document not executed", "config_path", path, "error", err)
		return nil
	}
	var result dsc.Result
	hash := ""
	err = candidate.err
	input := candidate.input
	if err == nil {
		input, err = readInput(ctx, input)
		if err == nil {
			input.Operation, err = configurationOperation(path, input.ConfigurationText)
		}
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
		if p.server == nil && p.startErr == nil {
			p.server, p.startErr = r.start(ctx)
			if p.startErr != nil {
				// A factory may return a typed nil session along with its error.
				p.server = nil
			}
		}
		if p.startErr != nil {
			result = dsc.StartFailure(input, p.startErr)
		} else {
			result, err = p.server.Execute(ctx, input)
			if err != nil {
				// The failed input is never retried; recovery is for the next document.
				if closeErr := p.server.Close(); closeErr != nil {
					p.closeErr = errors.Join(p.closeErr, fmt.Errorf("close unusable DSC session: %w", closeErr))
				}
				p.server = nil
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
	return nil
}
