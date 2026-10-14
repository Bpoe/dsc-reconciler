// Package results publishes complete latest-attempt envelopes.
package results

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
)

// Writer owns one results directory. Only one daemon may own a directory pair.
type Writer struct {
	dir string
}

// NewWriter prepares a private results directory and checks write/sync/replacement access.
// Existing directory permissions and ownership are not changed.
func NewWriter(dir string) (*Writer, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve results directory: %w", err)
	}
	if err := makeDirectory(dir); err != nil {
		return nil, fmt.Errorf("prepare results directory %q: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve results directory %q: %w", dir, err)
	}
	w := &Writer{dir: resolved}
	if err := w.probe(); err != nil {
		return nil, fmt.Errorf("probe results directory %q: %w", dir, err)
	}
	return w, nil
}

func (w *Writer) probe() (err error) {
	f, err := createTemp(w.dir)
	if err != nil {
		return err
	}
	paths := []string{f.Name()}
	defer func() {
		for _, path := range paths {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
		err = errors.Join(err, syncDirectory(w.dir))
	}()
	syncErr := f.Sync()
	closeErr := f.Close()
	if err = errors.Join(syncErr, closeErr); err != nil {
		return err
	}
	target, err := createTemp(w.dir)
	if err != nil {
		return err
	}
	paths = append(paths, target.Name())
	if err := target.Close(); err != nil {
		return err
	}
	_, err = replaceFile(f.Name(), target.Name())
	return err
}

// Destination maps a normalized relative source name to its mirrored result path.
func (w *Writer) Destination(name string) (string, error) {
	if name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00") {
		return "", fmt.Errorf("invalid relative source name %q", name)
	}
	name += ".result.json"
	local, err := filepath.Localize(name)
	if err != nil {
		return "", fmt.Errorf("invalid relative source name %q: %w", name, err)
	}
	path := filepath.Join(w.dir, local)
	for _, part := range strings.Split(name, "/") {
		if err := validateDestination(part, path); err != nil {
			return "", err
		}
	}
	return path, nil
}

func (w *Writer) prepareDirectory(dir string) error {
	relative, err := filepath.Rel(w.dir, dir)
	if err != nil {
		return err
	}
	if !filepath.IsLocal(relative) {
		return fmt.Errorf("result directory %q is outside results root", dir)
	}
	current := w.dir
	parts := []string{"."}
	if relative != "." {
		parts = append(parts, strings.Split(relative, string(filepath.Separator))...)
	}
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := makeDirectory(current); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("result directory %q is not a regular directory", current)
		}
	}
	return nil
}

// Write serializes the result and syncs a private temporary file in the target
// directory before replacing the old result.
// Context cancellation is checked between filesystem operations, not inside syscalls.
func (w *Writer) Write(ctx context.Context, result dsc.Result) (err error) {
	target, err := w.Destination(result.Configuration)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	content, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("serialize result %q: %w", target, err)
	}
	dir := filepath.Dir(target)
	if err := w.prepareDirectory(dir); err != nil {
		return fmt.Errorf("prepare result directory %q: %w", dir, err)
	}
	f, err := createTemp(dir)
	if err != nil {
		return err
	}
	temp := f.Name()
	closed, published := false, false
	defer func() {
		if !closed {
			err = errors.Join(err, f.Close())
		}
		if !published {
			if removeErr := os.Remove(temp); removeErr != nil {
				err = errors.Join(err, fmt.Errorf("remove temporary result %q: %w", temp, removeErr))
			}
		}
		if err != nil {
			err = fmt.Errorf("publish result %q: %w", target, err)
		}
	}()
	if _, err = f.Write(append(content, '\n')); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	err = f.Close()
	closed = true
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	published, err = replaceFile(temp, target)
	if err != nil {
		if published {
			return fmt.Errorf("replacement is visible but post-replacement cleanup failed: %w", err)
		}
		return err
	}
	if err = syncDirectory(dir); err != nil {
		return fmt.Errorf("replacement is visible but directory durability is uncertain: %w", err)
	}
	return nil
}

func createTemp(dir string) (*os.File, error) {
	f, err := os.CreateTemp(dir, ".dscd-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary result in %q: %w", dir, err)
	}
	if err = protectFile(f.Name()); err != nil {
		return nil, fmt.Errorf("protect temporary result: %w", errors.Join(err, f.Close(), os.Remove(f.Name())))
	}
	return f, nil
}
