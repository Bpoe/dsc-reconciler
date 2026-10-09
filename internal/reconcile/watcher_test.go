package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/fsnotify/fsnotify"
)

type fakeEvents struct {
	events chan fsnotify.Event
	errors chan error
	closed int
	add    func(string) error
}

func newFakeEvents() *fakeEvents {
	return &fakeEvents{events: make(chan fsnotify.Event), errors: make(chan error)}
}

func (f *fakeEvents) source() (eventSource, error) {
	return eventSource{
		events: f.events, errors: f.errors,
		add: func(dir string) error {
			if f.add != nil {
				return f.add(dir)
			}
			return nil
		},
		close: func() error { f.closed++; return nil },
	}, nil
}

func (f *fakeEvents) send(dir, name string, op fsnotify.Op) {
	f.events <- fsnotify.Event{Name: filepath.Join(dir, name), Op: op}
}

func TestCollectorFiltersAndCoalesces(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		pending := newPendingChanges()
		source := newFakeEvents()
		w, err := startDirectoryWatch(context.Background(), dir, source.source, pending)
		if err != nil {
			t.Fatal(err)
		}
		defer w.close()
		for _, event := range []struct {
			name string
			op   fsnotify.Op
		}{
			{"web.yaml", fsnotify.Create},
			{"web.yaml", fsnotify.Write},
			{"web.parameters.json", fsnotify.Write},
			{"second.json", fsnotify.Create | fsnotify.Write},
			{"notes.txt", fsnotify.Write},
			{".hidden.yaml", fsnotify.Create},
			{"web.yaml.tmp", fsnotify.Create},
			{"web.YAML", fsnotify.Write},
			{"web.yml", fsnotify.Create},
			{filepath.Join("nested", "inside.yaml"), fsnotify.Create},
			{"gone.yaml", fsnotify.Remove},
			{"old.yaml", fsnotify.Rename},
			{"attr.yaml", fsnotify.Chmod},
		} {
			source.send(dir, event.name, event.op)
		}
		synctest.Wait()
		want := map[string]map[string]struct{}{
			"web": changedNames("web.yaml", "web.parameters.json"), "second": changedNames("second.json"),
		}
		if got := pending.take(); !reflect.DeepEqual(got, want) {
			t.Fatalf("pending=%v, want %v", got, want)
		}
		if len(pending.wake) != 1 {
			t.Fatal("notifications did not coalesce")
		}
		// The already-full notification channel must not block new state or failure.
		source.send(dir, "later.yaml", fsnotify.Write)
		source.errors <- fsnotify.ErrEventOverflow
		synctest.Wait()
		if !errors.Is(w.failure(), fsnotify.ErrEventOverflow) || len(pending.take()) != 1 {
			t.Fatal("collector blocked behind scheduler or lost its error")
		}
	})
}

func TestCollectorWatchLoss(t *testing.T) {
	for _, kind := range []string{"events closed", "errors closed", "root removed", "root renamed"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				source := newFakeEvents()
				w, err := startDirectoryWatch(context.Background(), dir, source.source, newPendingChanges())
				if err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "events closed":
					close(source.events)
				case "errors closed":
					close(source.errors)
				case "root removed":
					source.events <- fsnotify.Event{Name: dir, Op: fsnotify.Remove}
				case "root renamed":
					source.events <- fsnotify.Event{Name: dir, Op: fsnotify.Rename}
				}
				synctest.Wait()
				if w.failure() == nil {
					t.Fatal("watch loss was ignored")
				}
				if err := w.close(); err != nil || source.closed != 1 {
					t.Fatalf("close=%v count=%d", err, source.closed)
				}
			})
		})
	}
}

func TestDirectoryWatchIdentity(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "inputs")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	source := newFakeEvents()
	w, err := startDirectoryWatch(context.Background(), dir, source.source, newPendingChanges())
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	if err := w.validate(dir); err != nil {
		t.Fatalf("unchanged directory: %v", err)
	}
	input(t, dir, "web.yaml")
	if err := w.validate(dir); err != nil {
		t.Fatalf("content change mistaken for identity change: %v", err)
	}
	if err := os.Rename(dir, filepath.Join(root, "old-inputs")); err != nil {
		t.Fatal(err)
	}
	if err := w.validate(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory: %v", err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := w.validate(dir); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("replacement directory: %v", err)
	}
}

func TestDirectoryWatchSetupFailure(t *testing.T) {
	for _, kind := range []string{"add", "replaced", "removed", "not directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "inputs")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			source := newFakeEvents()
			source.add = func(string) error {
				if kind == "add" {
					return errors.New("cannot add watch")
				}
				if err := os.Rename(dir, filepath.Join(root, "old")); err != nil {
					return err
				}
				switch kind {
				case "replaced":
					return os.Mkdir(dir, 0700)
				case "not directory":
					return os.WriteFile(dir, nil, 0600)
				}
				return nil
			}
			w, err := startDirectoryWatch(context.Background(), dir, source.source, newPendingChanges())
			if w != nil || err == nil || source.closed != 1 {
				t.Fatalf("watch=%v error=%v closes=%d", w, err, source.closed)
			}
		})
	}
}
