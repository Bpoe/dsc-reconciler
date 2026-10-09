package reconcile

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
	"github.com/fsnotify/fsnotify"
)

type attemptLog struct {
	mu    sync.Mutex
	names []string
	times []time.Time
}

func (l *attemptLog) add(name string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = append(l.names, name)
	l.times = append(l.times, time.Now())
	return len(l.names)
}

func (l *attemptLog) snapshot() ([]string, []time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.names...), append([]time.Time(nil), l.times...)
}

func TestRunTargetedEvents(t *testing.T) {
	for _, test := range []struct {
		name     string
		existing []string
		changed  string
		op       fsnotify.Op
		want     string
	}{
		{"create configuration", nil, "web.yaml", fsnotify.Create, "web.yaml"},
		{"modify configuration", []string{"web.json"}, "web.json", fsnotify.Write, "web.json"},
		{"replace configuration", []string{"web.yaml"}, "web.yaml", fsnotify.Create, "web.yaml"},
		{"create yaml parameters", []string{"web.json"}, "web.parameters.yaml", fsnotify.Create, "web.json"},
		{"modify yaml parameters", []string{"web.json", "web.parameters.yaml"}, "web.parameters.yaml", fsnotify.Write, "web.json"},
		{"replace json parameters", []string{"web.yaml", "web.parameters.json"}, "web.parameters.json", fsnotify.Create, "web.yaml"},
		{"orphan create", nil, "web.parameters.json", fsnotify.Create, ""},
		{"orphan modify", []string{"web.parameters.yaml"}, "web.parameters.yaml", fsnotify.Write, ""},
		{"unrelated", []string{"web.yaml"}, "notes.txt", fsnotify.Write, ""},
		{"temporary", []string{"web.yaml"}, "web.yaml.tmp", fsnotify.Create, ""},
		{"hidden", nil, ".hidden.yaml", fsnotify.Create, ""},
		{"yml", nil, "web.yml", fsnotify.Create, ""},
		{"uppercase", nil, "web.YAML", fsnotify.Create, ""},
		{"delete parameters", []string{"web.yaml", "web.parameters.json"}, "web.parameters.json", fsnotify.Remove, ""},
		{"delete configuration", []string{"web.yaml"}, "web.yaml", fsnotify.Remove, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			input(t, dir, "unrelated.yaml")
			for _, name := range test.existing {
				input(t, dir, name)
			}
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				source := newFakeEvents()
				var calls []dsc.Input
				client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
					calls = append(calls, in)
					return dsc.InputFailure(in, errors.New("controlled failure"))
				}}
				writer := &fakeWriter{}
				r := New(dir, time.Hour, client.start, writer, logger())
				r.watch = source.source
				go r.Run(ctx)
				synctest.Wait()
				calls, writer.results = nil, nil
				const text = `{"resources":[],"metadata":{"dscd":{"operation":"test"}}}`
				if test.op == fsnotify.Remove {
					if err := os.Remove(filepath.Join(dir, test.changed)); err != nil {
						t.Fatal(err)
					}
				} else {
					writeInput(t, dir, test.changed, text)
				}
				source.send(dir, test.changed, test.op)
				synctest.Wait()
				if test.want == "" {
					if len(calls) != 0 || len(writer.results) != 0 {
						t.Fatalf("unexpected targeted work: %+v", calls)
					}
				} else {
					if len(calls) != 1 || len(writer.results) != 1 || filepath.Base(calls[0].Configuration) != test.want {
						t.Fatalf("calls=%+v results=%+v", calls, writer.results)
					}
					if parameterName(test.changed) {
						if calls[0].ParametersText != text {
							t.Fatal("did not submit current parameter bytes")
						}
					} else if calls[0].ConfigurationText != text || calls[0].Operation != dsc.OperationTest {
						t.Fatal("did not submit current configuration/operation")
					}
				}
				cancel()
				synctest.Wait()
				if source.closed != 1 {
					t.Fatal("watcher not closed on shutdown")
				}
			})
		})
	}
}

func TestRunParameterFirst(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		source := newFakeEvents()
		var calls []dsc.Input
		client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
			calls = append(calls, in)
			return dsc.InputFailure(in, errors.New("controlled failure"))
		}}
		r := New(dir, time.Hour, client.start, &fakeWriter{}, logger())
		r.watch = source.source
		go r.Run(ctx)
		synctest.Wait()
		writeInput(t, dir, "web.parameters.json", `{"ready":true}`)
		source.send(dir, "web.parameters.json", fsnotify.Create)
		synctest.Wait()
		if len(calls) != 0 {
			t.Fatal("orphan sidecar executed")
		}
		input(t, dir, "web.yaml")
		source.send(dir, "web.yaml", fsnotify.Create)
		synctest.Wait()
		if len(calls) != 1 || calls[0].ParametersText != `{"ready":true}` {
			t.Fatalf("parameter-first calls: %+v", calls)
		}
	})
}

func TestTargetedWorkPreservesFullPassDeadline(t *testing.T) {
	for _, duration := range []time.Duration{time.Minute, 6 * time.Minute} {
		t.Run(duration.String(), func(t *testing.T) {
			dir := t.TempDir()
			input(t, dir, "a.yaml")
			input(t, dir, "b.yaml")
			synctest.Test(t, func(t *testing.T) {
				const interval = 5 * time.Minute
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				source := newFakeEvents()
				var attempts attemptLog
				blocked := make(chan struct{})
				client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
					if attempts.add(filepath.Base(in.Configuration)) == 3 {
						select {
						case <-blocked:
						case <-ctx.Done():
						}
					}
					return dsc.InputFailure(in, errors.New("controlled failure"))
				}}
				r := New(dir, interval, client.start, &fakeWriter{}, logger())
				r.watch = source.source
				go r.Run(ctx)
				synctest.Wait()
				start := time.Now()
				time.Sleep(time.Minute)
				source.send(dir, "a.yaml", fsnotify.Write)
				synctest.Wait()
				time.Sleep(duration)
				synctest.Wait()
				names, _ := attempts.snapshot()
				if len(names) != 3 {
					t.Fatal("overlapping periodic work")
				}
				if duration > interval {
					// A due full pass must precede this additional targeted work.
					source.send(dir, "b.yaml", fsnotify.Write)
				}
				close(blocked)
				synctest.Wait()
				if duration < interval {
					time.Sleep(interval - time.Minute - duration - time.Nanosecond)
					synctest.Wait()
					names, _ = attempts.snapshot()
					if len(names) != 3 {
						t.Fatal("periodic pass ran early")
					}
					time.Sleep(time.Nanosecond)
					synctest.Wait()
				}
				expected := start.Add(interval)
				wantNames := []string{"a.yaml", "b.yaml", "a.yaml", "a.yaml", "b.yaml"}
				if duration > interval {
					expected = start.Add(time.Minute + duration)
					wantNames = append(wantNames, "b.yaml")
				}
				names, times := attempts.snapshot()
				if !reflect.DeepEqual(names, wantNames) || !times[3].Equal(expected) {
					t.Fatalf("calls=%v times=%v; expected full pass at %v", names, times, expected)
				}
				time.Sleep(interval - time.Nanosecond)
				synctest.Wait()
				names, _ = attempts.snapshot()
				if len(names) != len(wantNames) {
					t.Fatal("catch-up pass or reset by targeted work")
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				names, _ = attempts.snapshot()
				if len(names) != len(wantNames)+2 {
					t.Fatal("periodic reconciliation stopped")
				}
			})
		})
	}
}

func TestCollectorDoesNotWaitForScheduler(t *testing.T) {
	for _, phase := range []string{"execute", "publish", "close"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				source := newFakeEvents()
				release := make(chan struct{})
				starts, closes, calls := 0, 0, 0
				var logs bytes.Buffer
				block := func(at string) {
					if phase == at && starts == 1 {
						select {
						case <-release:
						case <-ctx.Done():
						}
					}
				}
				writer := &fakeWriter{write: func(context.Context, dsc.Result) error { block("publish"); return nil }}
				r := New(dir, time.Hour, func(context.Context) (DSC, error) {
					if starts != closes {
						t.Error("started an overlapping DSC session")
					}
					starts++
					return &fakeSession{
						execute: func(_ context.Context, in dsc.Input) (dsc.Result, error) {
							calls++
							block("execute")
							return dsc.InputFailure(in, errors.New("controlled failure")), nil
						},
						close: func() error { block("close"); closes++; return nil },
					}, nil
				}, writer, slog.New(slog.NewJSONHandler(&logs, nil)))
				r.watch = source.source
				go r.Run(ctx)
				synctest.Wait()
				input(t, dir, "a.yaml")
				source.send(dir, "a.yaml", fsnotify.Create)
				synctest.Wait()
				input(t, dir, "b.yaml")
				consumed := make(chan struct{})
				go func() {
					for range 100 {
						source.send(dir, "a.yaml", fsnotify.Write)
					}
					source.send(dir, "b.yaml", fsnotify.Create)
					source.errors <- fsnotify.ErrEventOverflow
					close(consumed)
				}()
				synctest.Wait()
				select {
				case <-consumed:
				default:
					t.Fatal("collector waited for the blocked scheduler")
				}
				if starts != 1 || calls != 1 || closes != 0 {
					t.Fatalf("unsafe overlap: starts=%d calls=%d closes=%d", starts, calls, closes)
				}
				close(release)
				synctest.Wait()
				if starts != 3 || closes != 3 || calls != 3 || source.closed != 1 {
					t.Fatalf("lost/coalesced incorrectly: starts=%d calls=%d closes=%d watcher closes=%d", starts, calls, closes, source.closed)
				}
				if !strings.Contains(logs.String(), "periodic reconciliation continues") {
					t.Fatal("watcher failure was not reported")
				}
				cancel()
				synctest.Wait()
			})
		})
	}
}

func TestPendingBatchCoalescesBeforeDispatch(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		source := newFakeEvents()
		first, second := make(chan struct{}), make(chan struct{})
		var calls []dsc.Input
		client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
			calls = append(calls, in)
			switch filepath.Base(in.Configuration) {
			case "a.yaml":
				select {
				case <-first:
				case <-ctx.Done():
				}
			case "b.yaml":
				select {
				case <-second:
				case <-ctx.Done():
				}
			}
			return dsc.InputFailure(in, errors.New("controlled failure"))
		}}
		r := New(dir, time.Hour, client.start, &fakeWriter{}, logger())
		r.watch = source.source
		go r.Run(ctx)
		synctest.Wait()
		input(t, dir, "a.yaml")
		source.send(dir, "a.yaml", fsnotify.Create)
		synctest.Wait()
		for _, name := range []string{"b.yaml", "c.yaml"} {
			input(t, dir, name)
			source.send(dir, name, fsnotify.Create)
		}
		synctest.Wait()
		close(first)
		synctest.Wait()
		if len(calls) != 2 {
			t.Fatal("second batch did not start")
		}
		const updated = "resources: []\nmetadata: {dscd: {operation: test}}\n"
		writeInput(t, dir, "c.yaml", updated)
		source.send(dir, "c.yaml", fsnotify.Write)
		synctest.Wait()
		close(second)
		synctest.Wait()
		if len(calls) != 3 || filepath.Base(calls[2].Configuration) != "c.yaml" ||
			calls[2].ConfigurationText != updated {
			t.Fatalf("pending key duplicated or stale: %+v", calls)
		}
	})
}

func TestRunCancellationDuringTargetedExecution(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		source := newFakeEvents()
		calls, closes := 0, 0
		writer := &fakeWriter{write: func(ctx context.Context, _ dsc.Result) error {
			if ctx.Err() != nil {
				t.Error("canceled publication context")
			}
			return nil
		}}
		r := New(dir, time.Hour, func(context.Context) (DSC, error) {
			return &fakeSession{
				execute: func(ctx context.Context, in dsc.Input) (dsc.Result, error) {
					calls++
					<-ctx.Done()
					return dsc.InputFailure(in, ctx.Err()), ctx.Err()
				},
				close: func() error { closes++; return nil },
			}, nil
		}, writer, logger())
		r.watch = source.source
		done := make(chan struct{})
		go func() { r.Run(ctx); close(done) }()
		synctest.Wait()
		input(t, dir, "a.yaml")
		source.send(dir, "a.yaml", fsnotify.Create)
		synctest.Wait()
		input(t, dir, "b.yaml")
		source.send(dir, "b.yaml", fsnotify.Create)
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("shutdown did not join collector")
		}
		if calls != 1 || closes != 1 || source.closed != 1 || len(writer.results) != 1 ||
			writer.results[0].Outcome != "canceled" {
			t.Fatalf("calls=%d closes=%d watcher closes=%d results=%+v", calls, closes, source.closed, writer.results)
		}
	})
}

func TestRunWatcherRecovery(t *testing.T) {
	for _, kind := range []string{"setup failure", "overflow", "silent replacement", "missing then recreated"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "inputs")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			input(t, dir, "original.yaml")
			synctest.Test(t, func(t *testing.T) {
				const interval = 5 * time.Minute
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				sources := make(chan *fakeEvents, 4)
				var registrations atomic.Int32
				var attempts attemptLog
				client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
					attempts.add(filepath.Base(in.Configuration))
					return dsc.InputFailure(in, errors.New("controlled failure"))
				}}
				var logs bytes.Buffer
				r := New(dir, interval, client.start, &fakeWriter{}, slog.New(slog.NewJSONHandler(&logs, nil)))
				r.watch = func() (eventSource, error) {
					if registrations.Add(1) == 1 && kind == "setup failure" {
						return eventSource{}, errors.New("watch setup unavailable")
					}
					source := newFakeEvents()
					sources <- source
					return source.source()
				}
				go r.Run(ctx)
				synctest.Wait()
				names, _ := attempts.snapshot()
				if !reflect.DeepEqual(names, []string{"original.yaml"}) {
					t.Fatalf("startup: %v", names)
				}
				var first *fakeEvents
				if kind != "setup failure" {
					first = <-sources
				}
				if kind == "overflow" {
					first.errors <- fsnotify.ErrEventOverflow
					synctest.Wait()
				}
				if kind == "silent replacement" || kind == "missing then recreated" {
					if err := os.Rename(dir, filepath.Join(root, "old")); err != nil {
						t.Fatal(err)
					}
					if kind == "missing then recreated" {
						time.Sleep(interval)
						synctest.Wait()
						names, _ = attempts.snapshot()
						if first.closed != 1 || len(names) != 1 {
							t.Fatal("missing directory watch was not retired")
						}
					}
					if err := os.Mkdir(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				input(t, dir, "new.yaml")
				time.Sleep(interval - time.Nanosecond)
				synctest.Wait()
				names, _ = attempts.snapshot()
				if len(names) != 1 || registrations.Load() != 1 {
					t.Fatal("retry or periodic pass happened early")
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if registrations.Load() != 2 || len(sources) != 1 {
					t.Fatalf("watch was not recovered: %d", registrations.Load())
				}
				want := []string{"original.yaml", "new.yaml", "original.yaml"}
				if kind == "silent replacement" || kind == "missing then recreated" {
					want = []string{"original.yaml", "new.yaml"}
					if first.closed != 1 {
						t.Fatal("stale watcher not closed")
					}
				}
				names, _ = attempts.snapshot()
				if !reflect.DeepEqual(names, want) {
					t.Fatalf("periodic recovery=%v, want %v", names, want)
				}
				last := <-sources
				last.send(dir, "new.yaml", fsnotify.Write)
				synctest.Wait()
				names, _ = attempts.snapshot()
				if !reflect.DeepEqual(names, append(want, "new.yaml")) {
					t.Fatalf("replacement watcher not targeting: %v", names)
				}
				if !strings.Contains(logs.String(), "periodic reconciliation continues") {
					t.Fatal("watcher problem was not logged")
				}
				cancel()
				synctest.Wait()
				if last.closed != 1 {
					t.Fatal("recovered watcher not closed")
				}
			})
		})
	}
}
