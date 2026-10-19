// Package reconcile discovers documents and serially coordinates execution and publication.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
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
		if watcher != nil {
			if err := watcher.addTree(ctx, r.dir); err != nil {
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
	var batch []candidate
	var batchChanges map[string]inputChanges
	recoveryRequested, recoveryAttempted := false, false
	for {
		if ctx.Err() != nil {
			return
		}
		if watcher != nil {
			if err := watcher.failure(); err != nil {
				watchFailure(err)
				recoveryRequested = !recoveryAttempted
			}
		}
		periodicDue := !time.Now().Before(nextFull)
		if periodicDue || recoveryRequested {
			if recoveryRequested {
				// This full discovery covers pre-failure hints. Changes arriving
				// during recovery remain pending, just as during normal work.
				pending.take()
				pending.takeEntries()
				batch, batchChanges = nil, nil
			}
			recoveryAttempted = !periodicDue
			recoveryRequested = false
			fullPass()
			nextFull = time.Now().Add(r.interval)
			timer.Reset(r.interval)
			continue
		}
		if watcher != nil {
			if err := watcher.update(ctx, r.dir, pending); err != nil {
				watchFailure(err)
				recoveryRequested = !recoveryAttempted
				if recoveryRequested {
					continue
				}
			}
		}
		if len(batch) == 0 {
			batchChanges = pending.take()
			names := make(inputChanges)
			for _, changes := range batchChanges {
				for name, removed := range changes {
					names[name] = removed
				}
			}
			var err error
			batch, err = changedCandidates(r.dir, names)
			if err != nil {
				r.log.Error("targeted discovery failed", "config_path", r.dir, "error", err)
			}
		}
		if len(batch) != 0 {
			name := batch[0].name
			batch = batch[1:]
			// Rediscover at dispatch, coalescing updates to this configuration
			// while retaining notifications received during its attempt.
			changes := batchChanges[configurationBase(name)]
			for changed, removed := range pending.takeName(name) {
				changes[changed] = changes[changed] || removed
			}
			candidates, err := changedCandidates(r.dir, changes)
			for _, item := range candidates {
				if item.name == name {
					err = errors.Join(err, r.execute(ctx, []candidate{item}))
					break
				}
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				r.log.Error("targeted reconciliation failed", "config_path", r.dir, "error", err)
			}
			continue
		}
		var watchDone <-chan struct{}
		if watcher != nil {
			watchDone = watcher.failed
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
	candidates, err := discoverTree(ctx, r.dir)
	return errors.Join(err, r.execute(ctx, candidates))
}

func (r *Reconciler) changed(ctx context.Context, names inputChanges) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	candidates, err := changedCandidates(r.dir, names)
	return errors.Join(err, r.execute(ctx, candidates))
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
	configPath := candidate.input.Configuration
	target, err := r.writer.Destination(candidate.name)
	if err != nil {
		r.log.Error("unrepresentable result name; document not executed", "config_path", configPath, "error", err)
		return nil
	}
	var result dsc.Result
	hash := ""
	input := candidate.input
	err = checkInputDirectory(r.dir, path.Dir(candidate.name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var protected *os.File
	if err == nil {
		protected, err = openConfiguration(ctx, configPath)
		if errors.Is(err, errStaleInput) {
			r.log.Debug("configuration changed before reconciliation; attempt skipped", "config_path", configPath)
			return nil
		}
		if ctx.Err() != nil && protected == nil {
			return ctx.Err()
		}
		if protected != nil {
			defer func() {
				if err := protected.Close(); err != nil {
					r.log.Error("close protected configuration failed", "config_path", configPath, "error", err)
				}
			}()
		}
	}
	if err == nil {
		// Discovery is only a hint. Re-pair under protection so a publisher's
		// replacement cannot inherit the previous configuration's sidecar.
		err = checkInputDirectory(r.dir, path.Dir(candidate.name))
		if err == nil {
			items, scanErr := discoverDirectory(r.dir, filepath.Dir(configPath))
			err = scanErr
			if err == nil {
				found := false
				for _, item := range items {
					if item.name == candidate.name {
						input, err, found = item.input, item.err, true
						break
					}
				}
				if !found {
					return nil
				}
			}
		}
	}
	if err == nil {
		input.ConfigurationText, err = readInputContents(ctx, protected, maxInputBytes)
		if err != nil {
			err = fmt.Errorf("read reconciliation input %q: %w", configPath, err)
		}
		if err == nil && input.Parameters != "" {
			input.ParametersText, err = readInputFile(ctx, input.Parameters, maxInputBytes-len(input.ConfigurationText))
		}
		if err == nil {
			input.Operation, err = configurationOperation(configPath, input.ConfigurationText)
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
	result.Configuration = candidate.name
	result.Parameters = ""
	if input.Parameters != "" {
		result.Parameters = path.Join(path.Dir(candidate.name), filepath.Base(input.Parameters))
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
	r.log.Log(ctx, level, "DSC attempt completed", "config_path", configPath, "configuration", candidate.name, "outcome", result.Outcome,
		"duration", time.Duration(result.DurationMS)*time.Millisecond, "exit_code", result.ExitCode,
		"parameters_path", input.Parameters, "operation", result.Operation, "error_kind", kind)
	publication, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	err = r.writer.Write(publication, result)
	cancel()
	if err != nil {
		r.log.Error("result publication failed", "config_path", configPath, "result_path", target, "error", err)
	}
	return nil
}
