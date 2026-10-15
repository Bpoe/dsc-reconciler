//go:build windows

package reconcile

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/windows"
)

// windowsEvents owns one recursive root handle. Descendant registrations only
// update the scheduler's topology; holding their handles would block parent moves.
type windowsEvents struct {
	root       string
	directory  windows.Handle
	completion windows.Handle
	stopping   windows.Handle
	overlapped windows.Overlapped
	buffer     []uint32 // DWORD-aligned as required by the API.
	events     chan fsnotify.Event
	errors     chan error
	stop       chan struct{}
	done       chan struct{}
	once       sync.Once
	readErr    error
	closeErr   error
}

func newEventSource() (eventSource, error) {
	w := newWindowsEvents()
	return eventSource{events: w.events, errors: w.errors, add: w.add, remove: w.remove, close: w.close}, nil
}

func newWindowsEvents() *windowsEvents {
	return &windowsEvents{
		buffer: make([]uint32, 16384), // 64 KiB, including on local NTFS.
		events: make(chan fsnotify.Event), errors: make(chan error, 1),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// add and remove run only on the scheduler, never concurrently with close.
func (w *windowsEvents) add(directory string) (err error) {
	select {
	case <-w.stop:
		return fsnotify.ErrClosed
	default:
	}
	directory = filepath.Clean(directory)
	if w.root != "" {
		return w.contains(directory)
	}
	name, err := inputPath(directory)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, windows.CloseHandle(handle))
		}
	}()
	completion, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, windows.CloseHandle(completion))
		}
	}()
	stopping, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, windows.CloseHandle(stopping))
		}
	}()
	w.directory, w.completion, w.stopping = handle, completion, stopping
	w.overlapped.HEvent = completion
	if err := w.read(); err != nil {
		return err
	}
	w.root = directory
	go w.run()
	return nil
}

func (w *windowsEvents) contains(directory string) error {
	relative, err := filepath.Rel(w.root, directory)
	if err != nil {
		return err
	}
	if !filepath.IsLocal(relative) {
		return fmt.Errorf("directory %q is outside watched root %q", directory, w.root)
	}
	return nil
}

func (w *windowsEvents) remove(directory string) error {
	if filepath.Clean(directory) == w.root {
		return errors.New("recursive root watch must be closed, not removed")
	}
	return w.contains(directory)
}

func (w *windowsEvents) read() error {
	w.overlapped = windows.Overlapped{HEvent: w.completion}
	if err := windows.ResetEvent(w.completion); err != nil {
		return err
	}
	err := windows.ReadDirectoryChanges(w.directory, (*byte)(unsafe.Pointer(&w.buffer[0])),
		uint32(len(w.buffer)*4), true,
		windows.FILE_NOTIFY_CHANGE_FILE_NAME|windows.FILE_NOTIFY_CHANGE_DIR_NAME|windows.FILE_NOTIFY_CHANGE_LAST_WRITE,
		nil, &w.overlapped, 0)
	if errors.Is(err, windows.ERROR_IO_PENDING) {
		return nil
	}
	return err
}

func (w *windowsEvents) run() {
	err := w.receive()
	if err != nil {
		w.errors <- fmt.Errorf("watch configuration tree %q: %w", w.root, err)
	}
	close(w.done)
}

func (w *windowsEvents) receive() (err error) {
	pending := true // The first read is queued before add returns.
	defer func() {
		if pending {
			cancelErr := windows.CancelIoEx(w.directory, &w.overlapped)
			if errors.Is(cancelErr, windows.ERROR_NOT_FOUND) {
				cancelErr = nil
			}
			var size uint32
			waitErr := windows.GetOverlappedResult(w.directory, &w.overlapped, &size, true)
			if errors.Is(waitErr, windows.ERROR_OPERATION_ABORTED) {
				waitErr = nil
			}
			w.readErr = errors.Join(cancelErr, waitErr)
			err = errors.Join(err, w.readErr)
		}
	}()
	for {
		state, err := windows.WaitForMultipleObjects([]windows.Handle{w.stopping, w.completion}, false, windows.INFINITE)
		if err != nil {
			return err
		}
		if state == windows.WAIT_OBJECT_0 {
			return nil
		}
		if state != windows.WAIT_OBJECT_0+1 {
			return fmt.Errorf("unexpected directory notification wait result: %d", state)
		}
		var size uint32
		err = windows.GetOverlappedResult(w.directory, &w.overlapped, &size, false)
		pending = errors.Is(err, windows.ERROR_IO_INCOMPLETE)
		if errors.Is(err, windows.ERROR_NOTIFY_ENUM_DIR) {
			return fsnotify.ErrEventOverflow
		}
		if err != nil {
			return err
		}
		if size > uint32(len(w.buffer)*4) {
			return errors.New("directory notification exceeds buffer")
		}
		data := unsafe.Slice((*byte)(unsafe.Pointer(&w.buffer[0])), int(size))
		events, err := windowsNotifications(w.root, data)
		if err != nil {
			return err
		}
		// Rearm before delivery; decoding copied the names out of the I/O buffer.
		if err := w.read(); err != nil {
			return err
		}
		pending = true
		for _, event := range events {
			select {
			case w.events <- event:
			case <-w.stop:
				return nil
			}
		}
	}
}

func (w *windowsEvents) close() error {
	w.once.Do(func() {
		close(w.stop)
		if w.root != "" {
			signalErr := windows.SetEvent(w.stopping)
			<-w.done
			w.closeErr = errors.Join(signalErr, w.readErr, windows.CloseHandle(w.directory),
				windows.CloseHandle(w.completion), windows.CloseHandle(w.stopping))
		}
		close(w.events)
		close(w.errors)
	})
	return w.closeErr
}

// FILE_NOTIFY_INFORMATION is a chain of byte-sized records with UTF-16 names.
// A zero-byte completion means the kernel discarded notifications on overflow.
func windowsNotifications(root string, data []byte) ([]fsnotify.Event, error) {
	if len(data) == 0 {
		return nil, fsnotify.ErrEventOverflow
	}
	var events []fsnotify.Event
	for {
		if len(data) < 12 {
			return nil, errors.New("truncated directory notification header")
		}
		next := binary.LittleEndian.Uint32(data)
		action := binary.LittleEndian.Uint32(data[4:])
		size := binary.LittleEndian.Uint32(data[8:])
		if size == 0 || size%2 != 0 || uint64(size) > uint64(len(data)-12) {
			return nil, errors.New("invalid directory notification name length")
		}
		units := make([]uint16, size/2)
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(data[12+2*i:])
		}
		name := string(utf16.Decode(units))
		if !slices.Equal(utf16.Encode([]rune(name)), units) || strings.ContainsRune(name, '\x00') ||
			!filepath.IsLocal(name) || filepath.Clean(name) != name {
			return nil, errors.New("invalid directory notification path")
		}
		var op fsnotify.Op
		switch action {
		case windows.FILE_ACTION_ADDED, windows.FILE_ACTION_RENAMED_NEW_NAME:
			op = fsnotify.Create
		case windows.FILE_ACTION_REMOVED:
			op = fsnotify.Remove
		case windows.FILE_ACTION_MODIFIED:
			op = fsnotify.Write
		case windows.FILE_ACTION_RENAMED_OLD_NAME:
			op = fsnotify.Rename
		default:
			return nil, fmt.Errorf("unknown directory notification action: %d", action)
		}
		events = append(events, fsnotify.Event{Name: filepath.Join(root, name), Op: op})
		if next == 0 {
			return events, nil
		}
		if uint64(next) < 12+uint64(size) || next%4 != 0 || uint64(next) > uint64(len(data)-12) {
			return nil, errors.New("invalid directory notification offset")
		}
		data = data[next:]
	}
}
