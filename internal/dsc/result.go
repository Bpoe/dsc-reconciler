// Package dsc executes DSC and interprets only its top-level result envelope.
package dsc

import (
	"encoding/json"
	"path/filepath"
	"time"
)

// Failure classifies an unsuccessful attempt. Messages are not a stable interface.
type Failure struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Result is the version 1 latest-attempt contract. Nullable fields are always emitted.
type Result struct {
	SchemaVersion int             `json:"schemaVersion"`
	Configuration string          `json:"configuration"`
	StartedAt     time.Time       `json:"startedAt"`
	FinishedAt    time.Time       `json:"finishedAt"`
	DurationMS    int64           `json:"durationMs"`
	Outcome       string          `json:"outcome"`
	ExitCode      *int            `json:"exitCode"`
	DSCResult     json.RawMessage `json:"dscResult"`
	Error         *Failure        `json:"error"`
	Stderr        string          `json:"stderr"`
}

func newResult(path string, start time.Time) Result {
	return Result{SchemaVersion: 1, Configuration: filepath.Base(path), StartedAt: start.UTC(), Outcome: "succeeded"}
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

// InputFailure records a document that became ineligible or unreadable before execution.
func InputFailure(path string, err error) Result {
	start := time.Now()
	r := newResult(path, start)
	r.fail("input", err.Error())
	r.finish(start)
	return r
}
