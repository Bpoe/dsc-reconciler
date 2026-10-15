package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

type eventSource struct {
	events <-chan fsnotify.Event
	errors <-chan error
	add    func(string) error
	remove func(string) error
	close  func() error
}

// pendingChanges is authoritative; wake is only a nonblocking notification.
// Neither side holds mu across I/O, execution, logging, or watcher cleanup.
type pendingChanges struct {
	mu      sync.Mutex
	changes map[string]inputChanges
	entries map[string]fsnotify.Op
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

func (p *pendingChanges) add(name string, removed bool) {
	base, ok := changedBase(name)
	if !ok {
		return
	}
	p.mu.Lock()
	if p.changes == nil {
		p.changes = make(map[string]inputChanges)
	}
	if p.changes[base] == nil {
		p.changes[base] = make(inputChanges)
	}
	p.changes[base][name] = p.changes[base][name] || removed
	p.mu.Unlock()
	p.signal()
}

func (p *pendingChanges) take() map[string]inputChanges {
	p.mu.Lock()
	changes := p.changes
	p.changes = nil
	p.mu.Unlock()
	return changes
}

func (p *pendingChanges) takeName(name string) inputChanges {
	base := configurationBase(name)
	taken := make(inputChanges)
	p.mu.Lock()
	names := p.changes[base]
	for _, changed := range append(parameterNames(base), name) {
		if removed, ok := names[changed]; ok {
			taken[changed] = removed
			delete(names, changed)
		}
	}
	if len(names) == 0 {
		delete(p.changes, base)
	}
	p.mu.Unlock()
	return taken
}

// The collector cannot stat entries: retain topology hints for the scheduler.
// Arbitrary visible names can be directories, including names with extensions.
func (p *pendingChanges) entry(name string, op fsnotify.Op) {
	p.mu.Lock()
	if p.entries == nil {
		p.entries = make(map[string]fsnotify.Op)
	}
	p.entries[name] |= op
	p.mu.Unlock()
	p.signal()
}

func (p *pendingChanges) takeEntries() map[string]fsnotify.Op {
	p.mu.Lock()
	entries := p.entries
	p.entries = nil
	p.mu.Unlock()
	return entries
}

type directoryWatch struct {
	source      eventSource
	identity    os.FileInfo
	directories map[string]os.FileInfo // Owned only by the scheduler.
	cancel      context.CancelFunc
	failed      chan struct{}
	done        chan struct{}
	err         error // Written only by collect; read only after failed closes.
}

// Stat through a handle captures Windows file IDs now, not through a later
// pathname lookup by os.SameFile. Do not retain a handle across replacement.
func directoryIdentity(path string) (info os.FileInfo, err error) {
	f, err := openInputFile(path)
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
	dir = filepath.Clean(dir)
	before, err := directoryIdentity(dir)
	if err != nil {
		return nil, err
	}
	source, err := factory()
	if err != nil {
		return nil, err
	}
	watchCtx, cancel := context.WithCancel(ctx)
	w := &directoryWatch{source: source, directories: make(map[string]os.FileInfo),
		cancel: cancel, failed: make(chan struct{}), done: make(chan struct{})}
	go w.collect(watchCtx, dir, pending)
	if err := w.addTree(ctx, dir); err != nil {
		return nil, errors.Join(err, w.close())
	}
	w.identity = w.directories[dir]
	if !os.SameFile(before, w.identity) {
		return nil, errors.Join(errors.New("configuration directory changed while registering watch"), w.close())
	}
	return w, nil
}

func (w *directoryWatch) addTree(ctx context.Context, dir string) error {
	return walkDirectories(ctx, dir, func(directory string) error {
		if err := w.failure(); err != nil {
			return err
		}
		before, err := directoryIdentity(directory)
		if err != nil {
			return err
		}
		if previous, ok := w.directories[directory]; ok {
			if os.SameFile(previous, before) {
				return nil
			}
			if err := w.removeTree(directory); err != nil {
				return err
			}
		}
		if err := w.source.add(directory); err != nil {
			return fmt.Errorf("watch configuration directory %q: %w", directory, err)
		}
		after, err := directoryIdentity(directory)
		if err == nil && !os.SameFile(before, after) {
			err = fmt.Errorf("configuration directory %q changed while registering watch", directory)
		}
		if err != nil {
			return err
		}
		w.directories[directory] = after
		return nil
	})
}

func (w *directoryWatch) removeTree(dir string) error {
	var removeErr error
	for directory := range w.directories {
		if directory != dir && !strings.HasPrefix(directory, dir+string(filepath.Separator)) {
			continue
		}
		err := w.source.remove(directory)
		if err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
			removeErr = errors.Join(removeErr, fmt.Errorf("remove directory watch %q: %w", directory, err))
		}
		delete(w.directories, directory)
	}
	return removeErr
}

func (w *directoryWatch) update(ctx context.Context, root string, pending *pendingChanges) error {
	entries := pending.takeEntries()
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names) // Parents before descendants, not execution priority.
	var updateErr error
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return errors.Join(updateErr, err)
		}
		file := filepath.Join(root, filepath.FromSlash(name))
		op := entries[name]
		if op.Has(fsnotify.Remove | fsnotify.Rename) {
			updateErr = errors.Join(updateErr, w.removeTree(file))
		}
		if !op.Has(fsnotify.Create) {
			continue
		}
		if err := checkInputDirectory(root, path.Dir(name)); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				updateErr = errors.Join(updateErr, err)
			}
			continue
		}
		info, err := os.Lstat(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			updateErr = errors.Join(updateErr, err)
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		updateErr = errors.Join(updateErr, w.addTree(ctx, file))
		items, err := discoverTree(ctx, file)
		updateErr = errors.Join(updateErr, err)
		for _, item := range items {
			pending.add(path.Join(name, item.name), false)
		}
	}
	return updateErr
}

func (w *directoryWatch) collect(ctx context.Context, dir string, pending *pendingChanges) {
	defer close(w.done)
	defer func() {
		if w.err == nil {
			return
		}
		close(w.failed)
		// Add/Remove may be in progress on the scheduler. Keep the backend's
		// event sends unblocked until it can observe failure and close us.
		events, failures := w.source.events, w.source.errors
		for events != nil || failures != nil {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-events:
				if !ok {
					events = nil
				}
			case _, ok := <-failures:
				if !ok {
					failures = nil
				}
			}
		}
	}()
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
			file := filepath.Clean(event.Name)
			if file == dir && event.Has(fsnotify.Remove|fsnotify.Rename) {
				w.err = errors.New("watched configuration directory was removed or renamed")
				return
			}
			// fsnotify reports rename destinations as Create, not Rename.
			name, err := relativeName(dir, file)
			if err != nil || !visiblePath(name) {
				continue
			}
			removed := parameterName(name) && event.Has(fsnotify.Remove|fsnotify.Rename)
			if removed || event.Has(fsnotify.Create|fsnotify.Write) {
				pending.add(name, removed)
			}
			if event.Has(fsnotify.Create | fsnotify.Remove | fsnotify.Rename) {
				pending.entry(name, event.Op)
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
	case <-w.failed:
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
	for directory, identity := range w.directories {
		name, err := relativeName(dir, directory)
		if err != nil {
			return err
		}
		if err := checkInputDirectory(dir, name); err != nil {
			return err
		}
		current, err := directoryIdentity(directory)
		if err != nil {
			return err
		}
		if !os.SameFile(identity, current) {
			return fmt.Errorf("watched configuration directory %q identity changed", directory)
		}
	}
	return nil
}

func (w *directoryWatch) close() error {
	w.cancel()
	err := w.source.close()
	<-w.done
	return err
}
