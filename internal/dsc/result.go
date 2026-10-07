// Package dsc executes DSC and interprets only its top-level result envelope.
package dsc

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"
)

// Input holds source identity and immutable UTF-8 text captured by the reconciler.
// Paths identify inputs; the server receives only ConfigurationText and ParametersText.
type Input struct {
	Configuration     string
	Parameters        string
	ConfigurationText string
	ParametersText    string
}

// StartFailure records an input which could not execute because server startup failed.
func StartFailure(input Input, err error) Result {
	r := InputFailure(input, err)
	if r.Outcome != "canceled" {
		r.Error.Kind = "start"
	}
	return r
}

// Failure classifies an unsuccessful attempt. Messages are not a stable interface.
type Failure struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Result is the version 1 latest-attempt contract. Nullable fields are always emitted.
// ExitCode is only available when the server exited during a failed request.
// Stderr is empty because the shared server stream cannot be attributed to an attempt.
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
