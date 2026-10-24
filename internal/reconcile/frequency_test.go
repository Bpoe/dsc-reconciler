package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
	"github.com/Bpoe/dsc-reconciler/internal/results"
)

func TestPeriodicFrequencyAndRestart(t *testing.T) {
	for _, operation := range []dsc.Operation{dsc.OperationSet, dsc.OperationTest, dsc.OperationExport} {
		frequency := 15 * time.Minute
		if operation == dsc.OperationExport {
			frequency = 24 * time.Hour
		}
		for _, outcome := range []string{"succeeded", "failed", "canceled"} {
			t.Run(string(operation)+"/"+outcome, func(t *testing.T) {
				dir := t.TempDir()
				name := "team/inventory.yaml"
				text := fmt.Sprintf("metadata: {dscd: {operation: %s}}\nresources: []\n", operation)
				writeInput(t, dir, name, text)
				writer, err := results.NewWriter(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				synctest.Test(t, func(t *testing.T) {
					starts, calls, closes := 0, 0, 0
					start := func(context.Context) (DSC, error) {
						starts++
						return &fakeSession{
							execute: func(_ context.Context, in dsc.Input) (dsc.Result, error) {
								calls++
								if in.Operation != operation || in.ConfigurationText != text {
									t.Fatalf("submitted snapshot changed: %+v", in)
								}
								result := completedResult(in, outcome)
								// A long attempt must still wait from its completion, not its start.
								result.StartedAt = result.FinishedAt.Add(-48 * time.Hour)
								result.DurationMS = (48 * time.Hour).Milliseconds()
								return result, nil
							},
							close: func() error { closes++; return nil },
						}, nil
					}
					pass := func() {
						t.Helper()
						// A new reconciler has no in-memory last-attempt state.
						if err := New(dir, 5*time.Minute, start, writer, logger()).Pass(context.Background()); err != nil {
							t.Fatal(err)
						}
					}
					pass()
					if starts != 1 || calls != 1 || closes != 1 {
						t.Fatalf("first pass: starts=%d calls=%d closes=%d", starts, calls, closes)
					}
					checkpoint, err := writer.Read(context.Background(), name)
					if err != nil || checkpoint.Operation == nil || *checkpoint.Operation != operation ||
						checkpoint.SchemaVersion != 1 || checkpoint.Outcome != outcome {
						t.Fatalf("persisted result: %+v (%v)", checkpoint, err)
					}
					target, _ := writer.Destination(name)
					before, err := os.ReadFile(target)
					if err != nil {
						t.Fatal(err)
					}
					info, err := os.Stat(target)
					if err != nil {
						t.Fatal(err)
					}
					pass()
					time.Sleep(frequency - time.Nanosecond)
					pass()
					after, err := os.ReadFile(target)
					afterInfo, statErr := os.Stat(target)
					if err != nil || statErr != nil || !bytes.Equal(before, after) || !info.ModTime().Equal(afterInfo.ModTime()) ||
						starts != 1 || calls != 1 || closes != 1 {
						t.Fatalf("fresh result replaced or server started: starts=%d calls=%d closes=%d read=%v stat=%v", starts, calls, closes, err, statErr)
					}
					time.Sleep(time.Nanosecond)
					pass()
					if starts != 2 || calls != 2 || closes != 2 {
						t.Fatalf("due boundary: starts=%d calls=%d closes=%d", starts, calls, closes)
					}
					latest, err := writer.Read(context.Background(), name)
					if err != nil || latest.FinishedAt.Sub(checkpoint.FinishedAt) != frequency {
						t.Fatalf("due result: %+v (%v)", latest, err)
					}
				})
			})
		}
	}
}

func completedResult(in dsc.Input, outcome string) dsc.Result {
	result := dsc.Result{
		SchemaVersion: 1, Configuration: filepath.Base(in.Configuration), Operation: &in.Operation,
		StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(), Outcome: outcome,
	}
	if outcome != "succeeded" {
		result.Error = &dsc.Failure{Kind: "dsc", Message: "controlled failure"}
		if outcome == "canceled" {
			result.Error.Kind = "canceled"
		}
	}
	if in.Operation == dsc.OperationExport && outcome != "canceled" {
		result.DSCResult = json.RawMessage(fmt.Sprintf(`{"result":{"resources":[]},"messages":[],"hadErrors":%t}`, outcome == "failed"))
	}
	return result
}

func TestPeriodicChangedInputsForceExecution(t *testing.T) {
	for _, change := range []string{"configuration", "parameters", "parameters removed", "operation", "frequency"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			const original = "metadata: {dscd: {operation: export, frequency: 24h}}\nresources: []\n"
			config := writeInput(t, dir, "web.yaml", original)
			parameter := writeInput(t, dir, "web.parameters.json", `{"message":"original"}`)
			writer, err := results.NewWriter(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var calls []dsc.Input
			client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
				calls = append(calls, in)
				return completedResult(in, "succeeded")
			}}
			r := New(dir, 5*time.Minute, client.start, writer, logger())
			pass := func() {
				t.Helper()
				if err := r.Pass(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			pass()
			pass()
			if len(calls) != 1 {
				t.Fatal("fresh configuration executed again")
			}
			want := original
			switch change {
			case "configuration":
				want += "# changed bytes\n"
			case "parameters":
				writeInput(t, dir, "web.parameters.json", `{"message":"changed"}`)
			case "parameters removed":
				if err := os.Remove(parameter); err != nil {
					t.Fatal(err)
				}
			case "operation":
				want = strings.Replace(original, "export", "test", 1)
			case "frequency":
				want = strings.Replace(original, "24h", "12h", 1)
			}
			if err := os.WriteFile(config, []byte(want), 0600); err != nil {
				t.Fatal(err)
			}
			pass()
			if len(calls) != 2 || calls[1].ConfigurationText != want || inputHash(calls[0]) == inputHash(calls[1]) {
				t.Fatalf("changed input not submitted exactly: %+v", calls)
			}
			pass()
			if len(calls) != 2 {
				t.Fatal("new checkpoint was not used")
			}
		})
	}
}

func TestUnusablePreviousResultDoesNotSuppressExecution(t *testing.T) {
	for _, invalid := range []string{
		"truncated", "trailing JSON", "null", "array", "invalid timestamp", "future timestamp", "zero timestamp",
		"null timestamp", "missing timestamp", "invalid operation", "null operation", "missing operation", "different operation",
		"missing hash", "different hash", "wrong schema", "wrong identity", "wrong outcome", "negative duration",
		"finish before start", "duplicate field", "not a file",
	} {
		t.Run(invalid, func(t *testing.T) {
			dir := t.TempDir()
			const text = "metadata: {dscd: {operation: export}}\nresources: []\n"
			config := writeInput(t, dir, "web.yaml", text)
			writer, err := results.NewWriter(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			in := dsc.Input{Configuration: config, ConfigurationText: text, Operation: dsc.OperationExport}
			previous := completedResult(in, "succeeded")
			previous.InputHash = inputHash(in)
			data, err := json.Marshal(previous)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			switch invalid {
			case "invalid timestamp":
				fields["finishedAt"] = "PRIVATE-INVALID-TIME"
			case "future timestamp":
				fields["finishedAt"] = time.Now().Add(24 * time.Hour).Format(time.RFC3339Nano)
			case "zero timestamp":
				fields["finishedAt"] = time.Time{}.Format(time.RFC3339Nano)
			case "null timestamp":
				fields["finishedAt"] = nil
			case "missing timestamp":
				delete(fields, "finishedAt")
			case "invalid operation":
				fields["operation"] = "get"
			case "null operation":
				fields["operation"] = nil
			case "missing operation":
				delete(fields, "operation")
			case "different operation":
				fields["operation"] = "test"
			case "missing hash":
				delete(fields, "inputHash")
			case "different hash":
				fields["inputHash"] = "sha256:" + strings.Repeat("0", 64)
			case "wrong schema":
				fields["schemaVersion"] = 2
			case "wrong identity":
				fields["configuration"] = "other.yaml"
			case "wrong outcome":
				fields["outcome"] = "running"
			case "negative duration":
				fields["durationMs"] = -1
			case "finish before start":
				fields["startedAt"] = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
			}
			data, err = json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			switch invalid {
			case "truncated":
				data = data[:len(data)-1]
			case "trailing JSON":
				data = append(data, []byte("{}")...)
			case "null":
				data = []byte("null")
			case "array":
				data = []byte("[]")
			case "duplicate field":
				data = append([]byte(`{"operation":"set",`), data[1:]...)
			}
			target, _ := writer.Destination("web.yaml")
			if invalid == "not a file" {
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(target, data, 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
				calls++
				return completedResult(in, "succeeded")
			}}
			var logs bytes.Buffer
			r := New(dir, time.Minute, client.start, writer, slog.New(slog.NewJSONHandler(&logs, nil)))
			if err := r.Pass(context.Background()); err != nil || calls != 1 {
				t.Fatalf("invalid result suppressed execution: calls=%d err=%v", calls, err)
			}
			if strings.Contains(logs.String(), "PRIVATE-INVALID-TIME") {
				t.Fatal("previous result contents leaked into logs")
			}
		})
	}
}

func TestFreshConfigurationDoesNotBlockDueOrInvalidInputs(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "10-fresh.yaml")
	writer, err := results.NewWriter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		calls = append(calls, filepath.Base(in.Configuration))
		return completedResult(in, "succeeded")
	}}
	r := New(dir, time.Minute, client.start, writer, logger())
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	input(t, dir, "20-due.yaml")
	writeInput(t, dir, "30-invalid.yaml", "metadata: {dscd: {frequency: 0s}}\n")
	calls = nil
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "20-due.yaml" {
		t.Fatalf("skip changed execution ordering: %v", calls)
	}
	invalid, err := writer.Read(context.Background(), "30-invalid.yaml")
	if err != nil || invalid.Error == nil || invalid.Error.Kind != "input" || invalid.Operation != nil {
		t.Fatalf("invalid input did not publish a failure: %+v (%v)", invalid, err)
	}
}

func TestTargetedExecutionRefreshesFrequencyCheckpoint(t *testing.T) {
	for _, changed := range []string{"web.yaml", "web.parameters.json"} {
		t.Run(changed, func(t *testing.T) {
			dir := t.TempDir()
			writeInput(t, dir, "web.yaml", "metadata: {dscd: {operation: export, frequency: 1h}}\nresources: []\n")
			writeInput(t, dir, "web.parameters.json", "{}")
			writer, err := results.NewWriter(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
					calls++
					return completedResult(in, "succeeded")
				}}
				r := New(dir, 5*time.Minute, client.start, writer, logger())
				if err := r.Pass(context.Background()); err != nil {
					t.Fatal(err)
				}
				first, err := writer.Read(context.Background(), "web.yaml")
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(30 * time.Minute)
				// Even an unchanged snapshot must run immediately for a filesystem hint.
				if err := r.changed(context.Background(), changedNames(changed)); err != nil {
					t.Fatal(err)
				}
				targeted, err := writer.Read(context.Background(), "web.yaml")
				if err != nil || calls != 2 || targeted.InputHash != first.InputHash ||
					targeted.FinishedAt.Sub(first.FinishedAt) != 30*time.Minute {
					t.Fatalf("targeted checkpoint: %+v, calls=%d (%v)", targeted, calls, err)
				}
				time.Sleep(30 * time.Minute)
				if err := r.Pass(context.Background()); err != nil || calls != 2 {
					t.Fatalf("used pre-targeted checkpoint: calls=%d (%v)", calls, err)
				}
				time.Sleep(30 * time.Minute)
				if err := r.Pass(context.Background()); err != nil || calls != 3 {
					t.Fatalf("targeted checkpoint did not become due: calls=%d (%v)", calls, err)
				}
			})
		})
	}
}

func TestUnreadableResultDoesNotSuppressExecution(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "web.yaml")
	calls := 0
	client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		calls++
		return completedResult(in, "failed")
	}}
	writer := &fakeWriter{read: func(context.Context, string) (dsc.Result, error) {
		return dsc.Result{}, os.ErrPermission
	}}
	if err := New(dir, time.Minute, client.start, writer, logger()).Pass(context.Background()); err != nil || calls != 1 {
		t.Fatalf("unreadable checkpoint suppressed attempt: %d (%v)", calls, err)
	}
}

func TestDueReadCancellationStopsExecution(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "web.yaml")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &fakeWriter{read: func(context.Context, string) (dsc.Result, error) {
		cancel()
		return dsc.Result{}, context.Canceled
	}}
	start := func(context.Context) (DSC, error) {
		t.Fatal("started server after cancellation while reading a result")
		return nil, nil
	}
	if err := New(dir, time.Minute, start, writer, logger()).Pass(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
