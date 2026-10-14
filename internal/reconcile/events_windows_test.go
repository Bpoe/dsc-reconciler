//go:build windows

package reconcile

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/windows"
)

func notification(action uint32, name string) []byte {
	units := utf16.Encode([]rune(name))
	data := make([]byte, (12+2*len(units)+3)&^3)
	binary.LittleEndian.PutUint32(data[4:], action)
	binary.LittleEndian.PutUint32(data[8:], uint32(2*len(units)))
	for i, unit := range units {
		binary.LittleEndian.PutUint16(data[12+2*i:], unit)
	}
	return data
}

func TestWindowsNotificationDecoding(t *testing.T) {
	root := t.TempDir()
	var data []byte
	var want []fsnotify.Event
	for _, test := range []struct {
		action uint32
		name   string
		op     fsnotify.Op
	}{
		{windows.FILE_ACTION_ADDED, `team\web.yaml`, fsnotify.Create},
		{windows.FILE_ACTION_MODIFIED, `team\web.parameters.json`, fsnotify.Write},
		{windows.FILE_ACTION_REMOVED, `team\web.parameters.yaml`, fsnotify.Remove},
		{windows.FILE_ACTION_RENAMED_OLD_NAME, `team`, fsnotify.Rename},
		{windows.FILE_ACTION_RENAMED_NEW_NAME, `.retired`, fsnotify.Create},
		{windows.FILE_ACTION_ADDED, "unicode-\u00e9-\U0001f600.yaml", fsnotify.Create},
	} {
		record := notification(test.action, test.name)
		binary.LittleEndian.PutUint32(record, uint32(len(record)))
		data = append(data, record...)
		want = append(want, fsnotify.Event{Name: filepath.Join(root, test.name), Op: test.op})
	}
	// The last record terminates the chain.
	offset := 0
	for next := int(binary.LittleEndian.Uint32(data)); offset+next < len(data); next = int(binary.LittleEndian.Uint32(data[offset:])) {
		offset += next
	}
	binary.LittleEndian.PutUint32(data[offset:], 0)
	got, err := windowsNotifications(root, data)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded=%v error=%v, want %v", got, err, want)
	}
	if _, err := windowsNotifications(root, nil); !errors.Is(err, fsnotify.ErrEventOverflow) {
		t.Fatalf("zero-byte overflow: %v", err)
	}
}

func TestWindowsRejectsMalformedNotifications(t *testing.T) {
	for _, kind := range []string{
		"header", "odd length", "long length", "empty name", "small offset", "unaligned offset", "large offset",
		"unknown action", "absolute", "parent", "unclean", "nul", "invalid UTF16",
	} {
		t.Run(kind, func(t *testing.T) {
			data := notification(windows.FILE_ACTION_ADDED, `team\web.yaml`)
			switch kind {
			case "header":
				data = data[:11]
			case "odd length":
				binary.LittleEndian.PutUint32(data[8:], 3)
			case "long length":
				binary.LittleEndian.PutUint32(data[8:], ^uint32(1))
			case "empty name":
				binary.LittleEndian.PutUint32(data[8:], 0)
			case "small offset":
				binary.LittleEndian.PutUint32(data, 4)
			case "unaligned offset":
				binary.LittleEndian.PutUint32(data, 13)
			case "large offset":
				binary.LittleEndian.PutUint32(data, ^uint32(3))
			case "unknown action":
				binary.LittleEndian.PutUint32(data[4:], 999)
			case "absolute":
				data = notification(windows.FILE_ACTION_ADDED, `C:\outside.yaml`)
			case "parent":
				data = notification(windows.FILE_ACTION_ADDED, `..\outside.yaml`)
			case "unclean":
				data = notification(windows.FILE_ACTION_ADDED, `team\..\web.yaml`)
			case "nul":
				data = notification(windows.FILE_ACTION_ADDED, "x\x00.yaml")
			case "invalid UTF16":
				binary.LittleEndian.PutUint16(data[12:], 0xdc00)
			}
			if events, err := windowsNotifications(t.TempDir(), data); err == nil || len(events) != 0 {
				t.Fatalf("accepted malformed notification: %v, %v", events, err)
			}
		})
	}
}

func TestWindowsNativeNotificationOverflow(t *testing.T) {
	root := t.TempDir()
	w := newWindowsEvents()
	w.buffer = make([]uint32, 16) // Force one filename to exceed the native buffer.
	if err := w.add(root); err != nil {
		t.Fatal(err)
	}
	defer w.close()
	input(t, root, strings.Repeat("a", 100)+".yaml")
	select {
	case err := <-w.errors:
		if !errors.Is(err, fsnotify.ErrEventOverflow) {
			t.Fatalf("overflow classification: %v", err)
		}
	case event := <-w.events:
		t.Fatalf("undersized buffer returned a partial event: %+v", event)
	case <-time.After(5 * time.Second):
		t.Fatal("native notification overflow was not surfaced")
	}
}

func TestWindowsNativeOverflowRecoversReconciliation(t *testing.T) {
	root := t.TempDir()
	input(t, root, "original.yaml")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var registrations atomic.Int32
	attempts := make(chan dsc.Input, 32)
	r := New(root, time.Hour, fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		select {
		case attempts <- in:
		case <-ctx.Done():
		}
		return dsc.Result{Outcome: "succeeded"}
	}}.start, &fakeWriter{}, logger())
	r.watch = func() (eventSource, error) {
		w := newWindowsEvents()
		if registrations.Add(1) == 1 {
			w.buffer = make([]uint32, 16)
		}
		return eventSource{events: w.events, errors: w.errors, add: w.add, remove: w.remove, close: w.close}, nil
	}
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("recovered watcher did not shut down")
		}
	}()
	select {
	case <-attempts:
	case <-ctx.Done():
		t.Fatal("initial reconciliation did not run")
	}
	name := strings.Repeat("new", 30) + ".yaml"
	publishInput(t, root, name, `{"resources":[]}`)
	for {
		select {
		case in := <-attempts:
			if filepath.Base(in.Configuration) == name {
				if registrations.Load() != 2 {
					t.Fatalf("overflow did not replace the root watch: %d", registrations.Load())
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("overflow did not rediscover the missed configuration")
		}
	}
}

func TestWindowsEventSourceSetupFailure(t *testing.T) {
	root := t.TempDir()
	file := writeInput(t, root, "not-a-directory", "")
	for _, dir := range []string{filepath.Join(root, "missing"), file} {
		w := newWindowsEvents()
		if err := w.add(dir); err == nil {
			w.close()
			t.Fatal("invalid directory watch succeeded")
		}
		if err := w.close(); err != nil {
			t.Fatal(err)
		}
		for _, handle := range []windows.Handle{w.directory, w.completion, w.stopping} {
			if handle == 0 {
				continue
			}
			if _, err := windows.WaitForSingleObject(handle, 0); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
				t.Fatalf("setup failure leaked handle: %v", err)
			}
		}
	}
}

func TestWindowsEventSourceShutdown(t *testing.T) {
	for _, phase := range []string{"before start", "idle read", "blocked delivery", "failed read"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			input(t, root, "old.yaml")
			w := newWindowsEvents()
			if phase == "failed read" {
				w.buffer = make([]uint32, 16)
			}
			if phase != "before start" {
				if err := w.add(root); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "blocked delivery" {
				if err := os.Rename(filepath.Join(root, "old.yaml"), filepath.Join(root, "new.yaml")); err != nil {
					t.Fatal(err)
				}
				select {
				case <-w.events: // Leave the second rename record undrained.
				case err := <-w.errors:
					t.Fatal(err)
				case <-time.After(5 * time.Second):
					t.Fatal("rename notification not received")
				}
			}
			if phase == "failed read" {
				input(t, root, strings.Repeat("b", 100)+".yaml")
				select {
				case <-w.errors:
				case <-time.After(5 * time.Second):
					t.Fatal("overflow did not end the read loop")
				}
			}
			done := make(chan error, 1)
			go func() { done <- w.close() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("watcher did not cancel and join outstanding I/O")
			}
			if err := w.close(); err != nil {
				t.Fatalf("second close: %v", err)
			}
			if phase != "before start" {
				for _, handle := range []windows.Handle{w.directory, w.completion, w.stopping} {
					if _, err := windows.WaitForSingleObject(handle, 0); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
						t.Fatalf("handle not released: %v", err)
					}
				}
			}
			if _, ok := <-w.events; ok {
				t.Fatal("events channel not closed")
			}
			if err := w.add(root); !errors.Is(err, fsnotify.ErrClosed) {
				t.Fatalf("closed source accepted registration: %v", err)
			}
		})
	}
}

func TestWindowsInputSnapshotSharesRenameHandle(t *testing.T) {
	root := t.TempDir()
	file := writeInput(t, root, "web.yaml", `{"resources":[]}`)
	name, err := inputPath(file)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	got, err := readInputFile(t.Context(), file, maxInputBytes)
	if err != nil || got != `{"resources":[]}` {
		t.Fatalf("snapshot conflicted with producer's rename handle: %q, %v", got, err)
	}
}
