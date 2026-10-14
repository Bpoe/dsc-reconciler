package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
	"github.com/fsnotify/fsnotify"
)

func TestRecursiveWatchRegistrationAndLifecycle(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "existing/deep/web.yaml")
	input(t, dir, ".staging/hidden.yaml")
	input(t, dir, "existing/.hidden/ignored.json")
	synctest.Test(t, func(t *testing.T) {
		pending := newPendingChanges()
		source := newFakeEvents()
		watched := make(map[string]bool)
		source.add = func(directory string) error {
			name, err := relativeName(dir, directory)
			watched[name] = true
			return err
		}
		source.remove = func(directory string) error {
			name, err := relativeName(dir, directory)
			delete(watched, name)
			return err
		}
		w, err := startDirectoryWatch(context.Background(), dir, source.source, pending)
		if err != nil {
			t.Fatal(err)
		}
		defer w.close()
		assertWatches := func(want ...string) {
			t.Helper()
			var got []string
			for name := range watched {
				got = append(got, name)
			}
			sort.Strings(got)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("watches=%v, want %v", got, want)
			}
		}
		update := func(name string, op fsnotify.Op) {
			t.Helper()
			source.send(dir, name, op)
			synctest.Wait()
			if err := w.update(context.Background(), dir, pending); err != nil {
				t.Fatal(err)
			}
		}
		assertWatches(".", "existing", "existing/deep")
		if err := os.Rename(filepath.Join(dir, ".staging"), filepath.Join(dir, "visible")); err != nil {
			t.Fatal(err)
		}
		update("visible", fsnotify.Create)
		assertWatches(".", "existing", "existing/deep", "visible")
		if got := pending.take(); !reflect.DeepEqual(got, map[string]inputChanges{
			"visible/hidden": changedNames("visible/hidden.yaml"),
		}) {
			t.Fatalf("populated directory not discovered: %v", got)
		}
		if err := os.Rename(filepath.Join(dir, "existing"), filepath.Join(dir, ".retired")); err != nil {
			t.Fatal(err)
		}
		update("existing", fsnotify.Rename)
		assertWatches(".", "visible")
		update(".retired", fsnotify.Create)
		source.send(dir, ".retired/deep/web.yaml", fsnotify.Write)
		synctest.Wait()
		if got := pending.take(); len(got) != 0 {
			t.Fatalf("hidden directory triggered work: %v", got)
		}
		if err := os.Remove(filepath.Join(dir, "visible", "hidden.yaml")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, "visible")); err != nil {
			t.Fatal(err)
		}
		update("visible", fsnotify.Remove)
		assertWatches(".")
		input(t, dir, "new/deep/config.yaml")
		update("new", fsnotify.Create)
		assertWatches(".", "new", "new/deep")
		if got := pending.take(); !reflect.DeepEqual(got, map[string]inputChanges{
			"new/deep/config": changedNames("new/deep/config.yaml"),
		}) {
			t.Fatalf("new subtree not discovered: %v", got)
		}
	})
}

func TestNestedRunTargetsOnlyAffectedConfigurations(t *testing.T) {
	for _, kind := range []string{
		"create", "modify", "replace", "parameters create", "parameters modify",
		"parameters delete", "delete", "move", "populated directory", "unhide directory", "hide directory",
	} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			input(t, dir, "unrelated/10-preparation.yaml")
			input(t, dir, "team/30-app.yaml")
			input(t, dir, "other/30-app.yaml")
			if kind == "parameters modify" || kind == "parameters delete" {
				input(t, dir, "team/30-app.parameters.json")
			}
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				source := newFakeEvents()
				var attempts attemptLog
				client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
					name, _ := relativeName(dir, in.Configuration)
					attempts.add(name)
					return dsc.InputFailure(in, errors.New("controlled failure"))
				}}
				writer := &fakeWriter{}
				r := New(dir, time.Hour, client.start, writer, logger())
				r.watch = source.source
				go r.Run(ctx)
				synctest.Wait()
				attempts.reset()
				writer.reset()
				want := []string{"team/30-app.yaml"}
				switch kind {
				case "create":
					input(t, dir, "team/40-new.json")
					source.send(dir, "team/40-new.json", fsnotify.Create)
					want = []string{"team/40-new.json"}
				case "modify", "replace":
					input(t, dir, "team/30-app.yaml")
					op := fsnotify.Write
					if kind == "replace" {
						op = fsnotify.Create
					}
					source.send(dir, "team/30-app.yaml", op)
				case "parameters create", "parameters modify":
					input(t, dir, "team/30-app.parameters.json")
					source.send(dir, "team/30-app.parameters.json", fsnotify.Create|fsnotify.Write)
				case "parameters delete", "delete":
					name := "team/30-app.yaml"
					if kind == "parameters delete" {
						name = "team/30-app.parameters.json"
					}
					if err := os.Remove(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
						t.Fatal(err)
					}
					source.send(dir, name, fsnotify.Remove)
					if kind == "delete" {
						want = nil
					}
				case "move":
					if err := os.Rename(filepath.Join(dir, "team", "30-app.yaml"), filepath.Join(dir, "other", "35-moved.yaml")); err != nil {
						t.Fatal(err)
					}
					source.send(dir, "team/30-app.yaml", fsnotify.Rename)
					source.send(dir, "other/35-moved.yaml", fsnotify.Create)
					want = []string{"other/35-moved.yaml"}
				case "populated directory":
					input(t, dir, "new/deep/35-new.yaml")
					input(t, dir, "new/.hidden/ignored.yaml")
					source.send(dir, "new", fsnotify.Create)
					want = []string{"new/deep/35-new.yaml"}
				case "unhide directory":
					input(t, dir, ".staging/deep/35-new.yaml")
					if err := os.Rename(filepath.Join(dir, ".staging"), filepath.Join(dir, "new")); err != nil {
						t.Fatal(err)
					}
					source.send(dir, ".staging", fsnotify.Rename)
					source.send(dir, "new", fsnotify.Create)
					want = []string{"new/deep/35-new.yaml"}
				case "hide directory":
					if err := os.Rename(filepath.Join(dir, "team"), filepath.Join(dir, ".retired")); err != nil {
						t.Fatal(err)
					}
					source.send(dir, "team", fsnotify.Rename)
					source.send(dir, ".retired/30-app.yaml", fsnotify.Write)
					want = nil
				}
				synctest.Wait()
				calls, _ := attempts.snapshot()
				published := writer.snapshot()
				if !reflect.DeepEqual(calls, want) || len(published) != len(want) {
					t.Fatalf("targeted calls=%v results=%+v, want %v", calls, published, want)
				}
				for i, result := range published {
					if result.Configuration != want[i] {
						t.Fatalf("result identity=%q, want %q", result.Configuration, want[i])
					}
				}
				attempts.reset()
				time.Sleep(time.Hour)
				synctest.Wait()
				calls, _ = attempts.snapshot()
				all, err := discover(dir)
				if err != nil || !reflect.DeepEqual(calls, candidateNames(all)) {
					t.Fatalf("periodic calls=%v, want %v: %v", calls, candidateNames(all), err)
				}
			})
		})
	}
}

func TestNestedPendingBatchFilenameOrderAndInflightChanges(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		source := newFakeEvents()
		release := make(chan struct{})
		var calls []string
		starts, closes := 0, 0
		r := New(dir, time.Hour, func(context.Context) (DSC, error) {
			if starts != closes {
				t.Error("overlapping sessions")
			}
			starts++
			return &fakeSession{
				execute: func(_ context.Context, in dsc.Input) (dsc.Result, error) {
					name, _ := relativeName(dir, in.Configuration)
					calls = append(calls, name)
					if len(calls) == 1 {
						select {
						case <-release:
						case <-ctx.Done():
						}
					}
					return dsc.InputFailure(in, errors.New("controlled failure")), nil
				},
				close: func() error { closes++; return nil },
			}, nil
		}, &fakeWriter{}, logger())
		r.watch = source.source
		go r.Run(ctx)
		synctest.Wait()
		input(t, dir, "00-active.yaml")
		source.send(dir, "00-active.yaml", fsnotify.Create)
		synctest.Wait()
		for _, name := range []string{
			"10-managed/20-baseline.yaml", "20-security/10-baseline.yaml", "15-local.yaml",
			"a/a.yaml", "b/a-b.yaml", "b/same.yaml", "a/same.yaml",
		} {
			input(t, dir, name)
			source.send(dir, name, fsnotify.Write)
		}
		for range 20 {
			source.send(dir, "00-active.yaml", fsnotify.Write)
		}
		synctest.Wait()
		close(release)
		synctest.Wait()
		want := []string{"00-active.yaml", "00-active.yaml", "20-security/10-baseline.yaml", "15-local.yaml",
			"10-managed/20-baseline.yaml", "b/a-b.yaml", "a/a.yaml", "a/same.yaml", "b/same.yaml"}
		if !reflect.DeepEqual(calls, want) || starts != len(want) || closes != starts {
			t.Fatalf("calls=%v starts=%d closes=%d, want %v", calls, starts, closes, want)
		}
	})
}

func TestSidecarDisappearsWhileQueued(t *testing.T) {
	for _, notify := range []bool{false, true} {
		name := "stale write"
		if notify {
			name = "explicit removal"
		}
		t.Run(name, func(t *testing.T) { testSidecarDisappearsWhileQueued(t, notify) })
	}
}

func testSidecarDisappearsWhileQueued(t *testing.T, notify bool) {
	dir := t.TempDir()
	input(t, dir, "team/20-app.yaml")
	input(t, dir, "team/20-app.parameters.json")
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		first, second := make(chan struct{}), make(chan struct{})
		source := newFakeEvents()
		var calls []string
		client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
			name, _ := relativeName(dir, in.Configuration)
			calls = append(calls, name)
			if name == "team/20-app.yaml" && len(calls) > 1 && in.Parameters != "" {
				t.Error("removed parameters were still submitted")
			}
			var blocked <-chan struct{}
			switch name {
			case "00-block.yaml":
				blocked = first
			case "10-wait.yaml":
				blocked = second
			}
			if blocked != nil {
				select {
				case <-blocked:
				case <-ctx.Done():
				}
			}
			return dsc.Result{Outcome: "succeeded"}
		}}
		r := New(dir, time.Hour, client.start, &fakeWriter{}, logger())
		r.watch = source.source
		go r.Run(ctx)
		synctest.Wait()
		calls = nil
		input(t, dir, "00-block.yaml")
		source.send(dir, "00-block.yaml", fsnotify.Create)
		synctest.Wait()
		input(t, dir, "10-wait.yaml")
		source.send(dir, "10-wait.yaml", fsnotify.Create)
		source.send(dir, "team/20-app.parameters.json", fsnotify.Write)
		synctest.Wait()
		close(first)
		synctest.Wait()
		if err := os.Remove(filepath.Join(dir, "team", "20-app.parameters.json")); err != nil {
			t.Fatal(err)
		}
		if notify {
			source.send(dir, "team/20-app.parameters.json", fsnotify.Remove)
		}
		synctest.Wait()
		close(second)
		synctest.Wait()
		want := []string{"00-block.yaml", "10-wait.yaml"}
		if notify {
			want = append(want, "team/20-app.yaml")
		}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("deleted sidecar calls=%v, want %v", calls, want)
		}
	})
}

func TestPeriodicRecoveryOfNestedWatches(t *testing.T) {
	for _, kind := range []string{"missed directory", "silent replacement", "nested registration failure"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			input(t, dir, "nested/old.yaml")
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var sources []*fakeEvents
				var watched, attempts attemptLog
				client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
					name, _ := relativeName(dir, in.Configuration)
					attempts.add(name)
					return dsc.Result{Outcome: "succeeded"}
				}}
				r := New(dir, time.Hour, client.start, &fakeWriter{}, logger())
				r.watch = func() (eventSource, error) {
					source := newFakeEvents()
					sources = append(sources, source)
					source.add = func(directory string) error {
						name, _ := relativeName(dir, directory)
						if kind == "nested registration failure" && len(sources) == 1 && name == "nested" {
							return errors.New("watch limit")
						}
						watched.add(name)
						return nil
					}
					return source.source()
				}
				go r.Run(ctx)
				synctest.Wait()
				calls, _ := attempts.snapshot()
				if !reflect.DeepEqual(calls, []string{"nested/old.yaml"}) {
					t.Fatalf("watch failure suppressed initial discovery: %v", calls)
				}
				if kind == "silent replacement" {
					if err := os.Rename(filepath.Join(dir, "nested"), filepath.Join(dir, ".old")); err != nil {
						t.Fatal(err)
					}
				}
				name := "nested/new.yaml"
				if kind == "missed directory" {
					name = "new/deep/new.yaml"
				}
				input(t, dir, name)
				attempts.reset()
				watched.reset()
				time.Sleep(time.Hour)
				synctest.Wait()
				calls, _ = attempts.snapshot()
				all, err := discover(dir)
				if err != nil || !reflect.DeepEqual(calls, candidateNames(all)) {
					t.Fatalf("periodic fallback=%v, want %v: %v", calls, candidateNames(all), err)
				}
				registrations, _ := watched.snapshot()
				if len(registrations) == 0 {
					t.Fatal("full-pass boundary did not restore nested watches")
				}
				if kind != "missed directory" && (len(sources) != 2 || sources[0].closed != 1) {
					t.Fatal("stale/failed watcher was not retired and replaced")
				}
				attempts.reset()
				sources[len(sources)-1].send(dir, name, fsnotify.Write)
				synctest.Wait()
				calls, _ = attempts.snapshot()
				if !reflect.DeepEqual(calls, []string{name}) {
					t.Fatalf("recovered nested watch did not target: %v", calls)
				}
			})
		})
	}
}
