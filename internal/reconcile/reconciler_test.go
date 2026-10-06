package reconcile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dsc-reconciler/internal/dsc"
)

type fakeDSC struct {
	run func(context.Context, dsc.Input) dsc.Result
}

func (f fakeDSC) Execute(ctx context.Context, input dsc.Input) dsc.Result { return f.run(ctx, input) }

type fakeWriter struct {
	results []dsc.Result
	write   func(context.Context, dsc.Result) error
}

func (w *fakeWriter) Destination(name string) (string, error) {
	if strings.HasPrefix(name, "unrepresentable") {
		return "", errors.New("name too long")
	}
	return name + ".result.json", nil
}

func (w *fakeWriter) Write(ctx context.Context, result dsc.Result) error {
	w.results = append(w.results, result)
	if w.write != nil {
		return w.write(ctx, result)
	}
	return nil
}

func logger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func input(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("opaque document"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryAndOrdering(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"20-b.yaml", "10-a.yaml", "30-c.json", "40-d.yml", ".hidden.yaml", "a.YAML", "a.yaml.tmp", "notes.txt", "10-a.parameters.yaml", "orphan.parameters.json"} {
		input(t, dir, name)
	}
	if err := os.Mkdir(filepath.Join(dir, "directory.yaml"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "10-a.yaml"), filepath.Join(dir, "link.yaml")); err != nil {
		t.Logf("symlink filtering not exercised: %v", err)
	}
	got, err := discover(dir)
	var names []string
	for _, item := range got {
		names = append(names, filepath.Base(item.input.Configuration))
	}
	want := []string{"10-a.yaml", "20-b.yaml", "30-c.json"}
	if err != nil || !reflect.DeepEqual(names, want) {
		t.Fatalf("discovery = %v, %v", got, err)
	}
	empty, err := discover(t.TempDir())
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty = %v, %v", empty, err)
	}
}

func TestFailureContinuationAndRediscovery(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"20-b.yaml", "10-a.yaml", "unrepresentable.yaml"} {
		input(t, dir, name)
	}
	var calls []string
	writer := &fakeWriter{write: func(context.Context, dsc.Result) error { return errors.New("disk failure") }}
	client := fakeDSC{run: func(_ context.Context, input dsc.Input) dsc.Result {
		name := filepath.Base(input.Configuration)
		calls = append(calls, name)
		return dsc.Result{Configuration: name, Outcome: "failed", Error: &dsc.Failure{Kind: "exit"}}
	}}
	r := New(dir, time.Second, client, writer, logger())
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"10-a.yaml", "20-b.yaml"}) || len(writer.results) != 2 {
		t.Fatalf("calls=%v results=%v", calls, writer.results)
	}
	if err := os.Remove(filepath.Join(dir, "10-a.yaml")); err != nil {
		t.Fatal(err)
	}
	input(t, dir, "30-c.yaml")
	calls = nil
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"20-b.yaml", "30-c.yaml"}) {
		t.Fatalf("rediscovery = %v", calls)
	}
}

func TestInputChangedBeforeExecution(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "10-a.yaml")
	input(t, dir, "20-b.yaml")
	writer := &fakeWriter{}
	client := fakeDSC{run: func(_ context.Context, input dsc.Input) dsc.Result {
		if err := os.Remove(filepath.Join(dir, "20-b.yaml")); err != nil {
			t.Error(err)
		}
		return dsc.Result{Configuration: filepath.Base(input.Configuration), Outcome: "succeeded"}
	}}
	if err := New(dir, time.Second, client, writer, logger()).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(writer.results) != 2 || writer.results[1].Error.Kind != "input" {
		t.Fatalf("results = %+v", writer.results)
	}
}

func TestCancellationPublishesAndStops(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "10-a.yaml")
	input(t, dir, "20-b.yaml")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &fakeWriter{write: func(ctx context.Context, _ dsc.Result) error {
		if ctx.Err() != nil {
			t.Error("publication inherited canceled execution context")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("publication has no deadline")
		}
		return nil
	}}
	client := fakeDSC{run: func(_ context.Context, input dsc.Input) dsc.Result {
		cancel()
		return dsc.Result{Configuration: filepath.Base(input.Configuration), Outcome: "canceled", Error: &dsc.Failure{Kind: "canceled"}}
	}}
	err := New(dir, time.Second, client, writer, logger()).Pass(ctx)
	if !errors.Is(err, context.Canceled) || len(writer.results) != 1 {
		t.Fatalf("cancellation: results=%v err=%v", writer.results, err)
	}
}

func TestImmediatePeriodicNonOverlappingPasses(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "a.yaml")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	starts := make(chan struct{}, 4)
	release := make(chan struct{})
	var active atomic.Int32
	var overlap atomic.Bool
	client := fakeDSC{run: func(ctx context.Context, input dsc.Input) dsc.Result {
		if active.Add(1) != 1 {
			overlap.Store(true)
		}
		defer active.Add(-1)
		starts <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return dsc.Result{Configuration: filepath.Base(input.Configuration), Outcome: "succeeded"}
	}}
	done := make(chan struct{})
	go func() {
		New(dir, 5*time.Millisecond, client, &fakeWriter{}, logger()).Run(ctx)
		close(done)
	}()
	receive := func() {
		t.Helper()
		select {
		case <-starts:
		case <-time.After(3 * time.Second):
			t.Fatal("pass did not start")
		}
	}
	receive()
	select {
	case <-starts:
		t.Fatal("overlapping pass")
	case <-time.After(25 * time.Millisecond):
	}
	release <- struct{}{}
	receive()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not stop")
	}
	if active.Load() != 0 || overlap.Load() {
		t.Fatal("active/overlapping execution")
	}
}

func TestScanFailureAndCanceledStartup(t *testing.T) {
	writer := &fakeWriter{}
	client := fakeDSC{run: func(context.Context, dsc.Input) dsc.Result {
		t.Fatal("unexpected execution")
		return dsc.Result{}
	}}
	r := New(filepath.Join(t.TempDir(), "missing"), time.Second, client, writer, logger())
	if r.Pass(context.Background()) == nil {
		t.Fatal("scan failure was ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)
}
