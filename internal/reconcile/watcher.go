package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

type eventSource struct {
	events <-chan fsnotify.Event
	errors <-chan error
	add    func(string) error
	close  func() error
}

func newEventSource() (eventSource, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return eventSource{}, err
	}
	return eventSource{events: w.Events, errors: w.Errors, add: w.Add, close: w.Close}, nil
}

// pendingChanges is authoritative; wake is only a nonblocking notification.
// Neither side holds mu across I/O, execution, logging, or watcher cleanup.
type pendingChanges struct {
	mu      sync.Mutex
	changes map[string]map[string]struct{}
	wake    chan struct{}
}

func newPendingChanges() *pendingChanges {
	return &pendingChanges{wake: make(chan struct{}, 1)}
}

func (p *pendingChanges) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *pendingChanges) add(name string) {
	base, ok := changedBase(name)
	if !ok {
		return
	}
	p.mu.Lock()
	if p.changes == nil {
		p.changes = make(map[string]map[string]struct{})
	}
	if p.changes[base] == nil {
		p.changes[base] = make(map[string]struct{})
	}
	p.changes[base][name] = struct{}{}
	p.mu.Unlock()
	p.signal()
}

func (p *pendingChanges) take() map[string]map[string]struct{} {
	p.mu.Lock()
	changes := p.changes
	p.changes = nil
	p.mu.Unlock()
	return changes
}

func (p *pendingChanges) takeBase(base string) map[string]struct{} {
	p.mu.Lock()
	names := p.changes[base]
	delete(p.changes, base)
	p.mu.Unlock()
	return names
}

type directoryWatch struct {
	source   eventSource
	identity os.FileInfo
	cancel   context.CancelFunc
	done     chan struct{}
	err      error // Written only by collect; read only after done closes.
}

// Stat through a handle captures Windows file IDs now, not through a later
// pathname lookup by os.SameFile. Do not retain a handle across replacement.
func directoryIdentity(path string) (info os.FileInfo, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err = f.Stat()
	if err == nil && !info.IsDir() {
		err = fmt.Errorf("%q is not a directory", path)
	}
	return info, err
}

func startDirectoryWatch(ctx context.Context, dir string, factory func() (eventSource, error), pending *pendingChanges) (*directoryWatch, error) {
	before, err := directoryIdentity(dir)
	if err != nil {
		return nil, err
	}
	source, err := factory()
	if err != nil {
		return nil, err
	}
	if err := source.add(dir); err != nil {
		return nil, errors.Join(err, source.close())
	}
	after, err := directoryIdentity(dir)
	if err == nil && !os.SameFile(before, after) {
		err = errors.New("configuration directory changed while registering watch")
	}
	if err != nil {
		return nil, errors.Join(err, source.close())
	}
	watchCtx, cancel := context.WithCancel(ctx)
	w := &directoryWatch{source: source, identity: after, cancel: cancel, done: make(chan struct{})}
	go w.collect(watchCtx, filepath.Clean(dir), pending)
	return w, nil
}

func (w *directoryWatch) collect(ctx context.Context, dir string, pending *pendingChanges) {
	defer close(w.done)
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.source.events:
			if !ok {
				w.err = errors.New("filesystem event channel closed")
				return
			}
			path := filepath.Clean(event.Name)
			if path == dir && event.Has(fsnotify.Remove|fsnotify.Rename) {
				w.err = errors.New("watched configuration directory was removed or renamed")
				return
			}
			// fsnotify reports rename destinations as Create, not Rename.
			if filepath.Dir(path) == dir && event.Has(fsnotify.Create|fsnotify.Write) {
				pending.add(filepath.Base(path))
			}
		case err, ok := <-w.source.errors:
			if !ok {
				err = errors.New("filesystem error channel closed")
			} else if err == nil {
				err = errors.New("filesystem watcher reported an unspecified failure")
			}
			w.err = err
			return
		}
	}
}

func (w *directoryWatch) failure() error {
	select {
	case <-w.done:
		return w.err
	default:
		return nil
	}
}

func (w *directoryWatch) validate(dir string) error {
	current, err := directoryIdentity(dir)
	if err != nil {
		return err
	}
	if !os.SameFile(w.identity, current) {
		return errors.New("watched configuration directory identity changed")
	}
	return nil
}

func (w *directoryWatch) close() error {
	w.cancel()
	err := w.source.close()
	<-w.done
	return err
}
