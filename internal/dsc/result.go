// Package dsc executes DSC and interprets only its top-level result envelope.
package dsc

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"
)

// Input identifies an opaque configuration and its optional parameter file by path.
type Input struct {
	Configuration string
	Parameters    string
}

// Failure classifies an unsuccessful attempt. Messages are not a stable interface.
type Failure struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Result is the version 1 latest-attempt contract. Nullable fields are always emitted.
type Result struct {
	SchemaVersion int             `json:"schemaVersion"`
	Configuration string          `json:"configuration"`
	Parameters    string          `json:"parameters,omitempty"`
	InputHash     string          `json:"inputHash,omitempty"`
	StartedAt     time.Time       `json:"startedAt"`
	FinishedAt    time.Time       `json:"finishedAt"`
	DurationMS    int64           `json:"durationMs"`
	Outcome       string          `json:"outcome"`
	ExitCode      *int            `json:"exitCode"`
	DSCResult     json.RawMessage `json:"dscResult"`
	Error         *Failure        `json:"error"`
	Stderr        string          `json:"stderr"`
}

func newResult(input Input, start time.Time) Result {
	r := Result{SchemaVersion: 1, Configuration: filepath.Base(input.Configuration), StartedAt: start.UTC(), Outcome: "succeeded"}
	if input.Parameters != "" {
		r.Parameters = filepath.Base(input.Parameters)
	}
	return r
}

func (r *Result) finish(start time.Time) {
	end := time.Now()
	r.FinishedAt = end.UTC()
	r.DurationMS = end.Sub(start).Milliseconds()
}

func (r *Result) fail(kind, message string) {
	r.Outcome = "failed"
	if kind == "canceled" {
		r.Outcome = "canceled"
	}
	r.Error = &Failure{Kind: kind, Message: message}
}

// InputFailure records an invalid or unreadable input, or cancellation before execution.
func InputFailure(input Input, err error) Result {
	start := time.Now()
	r := newResult(input, start)
	kind := "input"
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		kind = "canceled"
	}
	r.fail(kind, err.Error())
	r.finish(start)
	return r
}
