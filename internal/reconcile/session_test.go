package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
)

type fakeSession struct {
	execute func(context.Context, dsc.Input) (dsc.Result, error)
	close   func() error
}

func (s *fakeSession) Execute(ctx context.Context, in dsc.Input) (dsc.Result, error) {
	return s.execute(ctx, in)
}
func (s *fakeSession) Close() error { return s.close() }

func TestPassSessionLifetimeAndRecovery(t *testing.T) {
	for _, failure := range []string{"none", "dsc", "protocol", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"20-b.json", "10-a.yaml", "30-c.yaml"} {
				input(t, dir, name)
			}
			starts, closes := 0, 0
			var calls []string
			start := func(context.Context) (DSC, error) {
				if starts != closes {
					t.Fatal("previous session still active at restart")
				}
				starts++
				sessionID := starts
				return &fakeSession{
					execute: func(_ context.Context, in dsc.Input) (dsc.Result, error) {
						calls = append(calls, filepath.Base(in.Configuration))
						result := dsc.Result{Configuration: filepath.Base(in.Configuration), Outcome: "succeeded"}
						if strings.HasSuffix(in.Configuration, "10-a.yaml") && failure != "none" {
							result.Outcome = "failed"
							result.Error = &dsc.Failure{Kind: "dsc", Message: "test failure"}
							if failure == "protocol" || failure == "timeout" {
								if failure == "timeout" {
									result.Outcome = "canceled"
									result.Error.Kind = "canceled"
								}
								return result, errors.New("session unusable")
							}
						}
						if sessionID != starts {
							t.Fatal("using obsolete session")
						}
						return result, nil
					},
					close: func() error { closes++; return nil },
				}, nil
			}
			writer := &fakeWriter{}
			r := New(dir, time.Second, start, writer, logger())
			for pass := 1; pass <= 2; pass++ {
				calls = nil
				if err := r.Pass(context.Background()); err != nil {
					t.Fatal(err)
				}
				expected := pass
				if failure == "protocol" || failure == "timeout" {
					expected *= 2
				}
				if starts != expected || closes != starts || !reflect.DeepEqual(calls, []string{"10-a.yaml", "20-b.json", "30-c.yaml"}) {
					t.Fatalf("starts=%d closes=%d calls=%v", starts, closes, calls)
				}
			}
		})
	}
}

func TestEmptyPassAndStartupFailure(t *testing.T) {
	dir := t.TempDir()
	starts := 0
	start := func(context.Context) (DSC, error) { starts++; return nil, errors.New("initialize failed") }
	writer := &fakeWriter{}
	r := New(dir, time.Second, start, writer, logger())
	if err := r.Pass(context.Background()); err != nil || starts != 0 {
		t.Fatal("empty pass started server")
	}
	input(t, dir, "a.yaml")
	input(t, dir, "b.json")
	for pass := 1; pass <= 2; pass++ {
		if err := r.Pass(context.Background()); err == nil || !strings.Contains(err.Error(), "initialize failed") {
			t.Fatal("missing pass failure")
		}
		if starts != pass {
			t.Fatal("startup retried per document")
		}
	}
	for _, result := range writer.results {
		if result.Error == nil || result.Error.Kind != "start" || result.Outcome != "failed" {
			t.Fatalf("startup result: %+v", result)
		}
	}
}

func TestExactSnapshotSubmittedAfterReplacement(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "a.yaml")
	params := filepath.Join(dir, "a.parameters.json")
	const original = "metadata:\n  dscd:\n    operation: test\nresources: []\n"
	for path, text := range map[string]string{config: original, params: "old parameters\n"} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	expected := dsc.Input{Configuration: config, Parameters: params, Operation: dsc.OperationTest, ConfigurationText: original, ParametersText: "old parameters\n"}
	writer := &fakeWriter{}
	start := func(context.Context) (DSC, error) {
		// Startup occurs after the snapshot. Neither pathname may be reopened for this attempt.
		if err := os.Remove(config); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(params); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config, []byte("metadata:\n  dscd:\n    operation: set\nresources: []\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(params, []byte("new parameters"), 0600); err != nil {
			t.Fatal(err)
		}
		return fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
			if in != expected {
				t.Fatalf("submitted different bytes: %+v", in)
			}
			return dsc.Result{Configuration: "a.yaml", Outcome: "succeeded"}
		}}, nil
	}
	if err := New(dir, time.Second, start, writer, logger()).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(writer.results) != 1 || writer.results[0].InputHash != inputHash(expected) {
		t.Fatal("hash does not match submitted bytes")
	}
}

func TestCancellationClosesSession(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "a.yaml")
	input(t, dir, "b.yaml")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls, closes := 0, 0
	start := func(context.Context) (DSC, error) {
		return &fakeSession{
			execute: func(_ context.Context, in dsc.Input) (dsc.Result, error) {
				calls++
				cancel()
				return dsc.InputFailure(in, context.Canceled), context.Canceled
			},
			close: func() error { closes++; return nil },
		}, nil
	}
	writer := &fakeWriter{}
	if err := New(dir, time.Second, start, writer, logger()).Pass(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if calls != 1 || closes != 1 || len(writer.results) != 1 {
		t.Fatalf("calls=%d closes=%d results=%d", calls, closes, len(writer.results))
	}
}
