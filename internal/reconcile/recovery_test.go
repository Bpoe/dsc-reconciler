package reconcile

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
	"github.com/fsnotify/fsnotify"
)

func TestWatchFailureRequestsBoundedSerialRecovery(t *testing.T) {
	for _, kind := range []string{"overflow", "backend failure", "events closed", "root renamed"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			input(t, dir, "10-original.yaml")
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				sources := make(chan *fakeEvents, 8)
				var attempts attemptLog
				blocked := make(chan struct{})
				starts, closes := 0, 0
				r := New(dir, time.Hour, func(context.Context) (DSC, error) {
					if starts != closes {
						t.Error("recovery overlapped an active session")
					}
					starts++
					return &fakeSession{
						execute: func(_ context.Context, in dsc.Input) (dsc.Result, error) {
							if attempts.add(filepath.Base(in.Configuration)) == 2 {
								select {
								case <-blocked:
								case <-ctx.Done():
								}
							}
							return dsc.Result{Outcome: "succeeded"}, nil
						},
						close: func() error { closes++; return nil },
					}, nil
				}, &fakeWriter{}, logger())
				r.watch = func() (eventSource, error) {
					source := newFakeEvents()
					sources <- source
					return source.source()
				}
				go r.Run(ctx)
				synctest.Wait()
				first := <-sources
				first.send(dir, "10-original.yaml", fsnotify.Write)
				synctest.Wait()
				// No notification for this new input: recovery must rediscover it.
				input(t, dir, "20-missed.yaml")
				switch kind {
				case "overflow":
					first.errors <- fsnotify.ErrEventOverflow
				case "backend failure":
					first.errors <- errors.New("native read failed")
				case "events closed":
					close(first.events)
				case "root renamed":
					first.events <- fsnotify.Event{Name: dir, Op: fsnotify.Rename}
				}
				synctest.Wait()
				if starts != 2 || closes != 1 || len(sources) != 0 {
					t.Fatal("failure interrupted active execution")
				}
				close(blocked)
				synctest.Wait()
				calls, _ := attempts.snapshot()
				if !reflect.DeepEqual(calls, []string{"10-original.yaml", "10-original.yaml", "10-original.yaml", "20-missed.yaml"}) ||
					starts != 3 || closes != 3 || first.closed != 1 || len(sources) != 1 {
					t.Fatalf("recovery: calls=%v starts=%d closes=%d registrations=%d", calls, starts, closes, len(sources))
				}
				second := <-sources
				second.errors <- fsnotify.ErrEventOverflow
				synctest.Wait()
				if len(sources) != 0 || second.closed != 1 {
					t.Fatal("persistent failure caused an immediate retry loop")
				}
				time.Sleep(time.Hour - time.Nanosecond)
				synctest.Wait()
				calls, _ = attempts.snapshot()
				if len(calls) != 4 || len(sources) != 0 {
					t.Fatal("periodic fallback ran early")
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				calls, _ = attempts.snapshot()
				if len(calls) != 6 || len(sources) != 1 {
					t.Fatalf("periodic retry stopped: calls=%v registrations=%d", calls, len(sources))
				}
				last := <-sources
				cancel()
				synctest.Wait()
				if last.closed != 1 {
					t.Fatal("recovered watcher leaked on shutdown")
				}
			})
		})
	}
}
