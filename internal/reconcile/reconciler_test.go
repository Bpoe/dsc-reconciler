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
	"testing"
	"testing/synctest"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
)

type fakeDSC struct {
	run func(context.Context, dsc.Input) dsc.Result
}

func (f fakeDSC) Execute(ctx context.Context, input dsc.Input) (dsc.Result, error) {
	return f.run(ctx, input), nil
}
func (f fakeDSC) Close() error                       { return nil }
func (f fakeDSC) start(context.Context) (DSC, error) { return f, nil }

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
	r := New(dir, time.Second, client.start, writer, logger())
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
	if err := New(dir, time.Second, client.start, writer, logger()).Pass(context.Background()); err != nil {
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
	err := New(dir, time.Second, client.start, writer, logger()).Pass(ctx)
	if !errors.Is(err, context.Canceled) || len(writer.results) != 1 {
		t.Fatalf("cancellation: results=%v err=%v", writer.results, err)
	}
}

func TestRunDelayAfterPass(t *testing.T) {
	for _, duration := range []time.Duration{0, 2 * time.Minute, 12 * time.Minute} {
		t.Run(duration.String(), func(t *testing.T) {
			dir := t.TempDir()
			input(t, dir, "a.yaml")
			synctest.Test(t, func(t *testing.T) {
				const interval = 5 * time.Minute
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				starts := make(chan time.Time, 4)
				finishes := make(chan time.Time, 4)
				executed := make(chan struct{})
				published := make(chan struct{})
				client := fakeDSC{run: func(ctx context.Context, input dsc.Input) dsc.Result {
					starts <- time.Now()
					select {
					case <-executed:
					case <-ctx.Done():
					}
					return dsc.Result{Configuration: filepath.Base(input.Configuration), Outcome: "succeeded"}
				}}
				writer := &fakeWriter{write: func(context.Context, dsc.Result) error {
					select {
					case <-published:
					case <-ctx.Done():
					}
					finishes <- time.Now()
					return nil
				}}
				done := make(chan struct{})
				start := time.Now()
				go func() {
					New(dir, interval, client.start, writer, logger()).Run(ctx)
					close(done)
				}()
				synctest.Wait()
				if len(starts) != 1 || !(<-starts).Equal(start) {
					t.Fatal("first pass did not start immediately")
				}

				time.Sleep(duration)
				synctest.Wait()
				if len(starts) != 0 {
					t.Fatal("overlapping execution")
				}
				close(executed)
				synctest.Wait()
				// Completing DSC alone does not complete a pass: publication must finish too.
				time.Sleep(3 * time.Second)
				synctest.Wait()
				if len(starts) != 0 || len(finishes) != 0 {
					t.Fatal("pass completed before publication")
				}
				close(published)
				synctest.Wait()
				if len(finishes) != 1 || len(starts) != 0 {
					t.Fatal("completed pass triggered an immediate catch-up pass")
				}
				finished := <-finishes
				time.Sleep(interval - time.Nanosecond)
				synctest.Wait()
				if len(starts) != 0 {
					t.Fatal("second pass started before a full post-pass interval")
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if len(starts) != 1 {
					t.Fatal("second pass did not start after the interval")
				}
				if got := (<-starts).Sub(finished); got != interval {
					t.Fatalf("post-pass delay = %s, want %s", got, interval)
				}
				cancel()
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("loop did not stop")
				}
			})
		})
	}
}

func TestRunCancellationAfterPass(t *testing.T) {
	for _, when := range []string{"during wait", "at pass completion"} {
		t.Run(when, func(t *testing.T) {
			dir := t.TempDir()
			input(t, dir, "a.yaml")
			synctest.Test(t, func(t *testing.T) {
				const interval = 5 * time.Minute
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := make(chan struct{}, 4)
				client := fakeDSC{run: func(_ context.Context, input dsc.Input) dsc.Result {
					calls <- struct{}{}
					return dsc.Result{Configuration: filepath.Base(input.Configuration), Outcome: "succeeded"}
				}}
				writer := &fakeWriter{write: func(context.Context, dsc.Result) error {
					if when == "at pass completion" {
						cancel()
					}
					return nil
				}}
				done := make(chan struct{})
				go func() {
					New(dir, interval, client.start, writer, logger()).Run(ctx)
					close(done)
				}()
				synctest.Wait()
				if len(calls) != 1 || len(writer.results) != 1 {
					t.Fatal("initial pass did not complete")
				}
				if when == "during wait" {
					select {
					case <-done:
						t.Fatal("loop exited before cancellation")
					default:
					}
					time.Sleep(time.Minute)
				}
				canceledAt := time.Now()
				cancel()
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("cancellation did not promptly stop the loop")
				}
				if !time.Now().Equal(canceledAt) {
					t.Fatal("cancellation waited for the interval")
				}
				time.Sleep(2 * interval)
				synctest.Wait()
				if len(calls) != 1 {
					t.Fatal("another pass started after cancellation")
				}
			})
		})
	}
}

func TestRunRetriesPassFailureAfterInterval(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "initially missing")
	synctest.Test(t, func(t *testing.T) {
		const interval = 5 * time.Minute
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := make(chan struct{}, 4)
		client := fakeDSC{run: func(_ context.Context, input dsc.Input) dsc.Result {
			calls <- struct{}{}
			return dsc.Result{Configuration: filepath.Base(input.Configuration), Outcome: "succeeded"}
		}}
		go New(dir, interval, client.start, &fakeWriter{}, logger()).Run(ctx)
		synctest.Wait()
		if len(calls) != 0 {
			t.Fatal("executed a document in an unreadable directory")
		}
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		input(t, dir, "a.yaml")
		time.Sleep(interval - time.Nanosecond)
		synctest.Wait()
		if len(calls) != 0 {
			t.Fatal("retried before the interval")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if len(calls) != 1 {
			t.Fatal("pass failure prevented the scheduled retry")
		}
	})
}

func TestScanFailureAndCanceledStartup(t *testing.T) {
	writer := &fakeWriter{}
	client := fakeDSC{run: func(context.Context, dsc.Input) dsc.Result {
		t.Fatal("unexpected execution")
		return dsc.Result{}
	}}
	r := New(filepath.Join(t.TempDir(), "missing"), time.Second, client.start, writer, logger())
	if r.Pass(context.Background()) == nil {
		t.Fatal("scan failure was ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)
}
