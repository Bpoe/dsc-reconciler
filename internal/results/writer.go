// Package results publishes complete latest-attempt envelopes.
package results

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"dsc-reconciler/internal/dsc"
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
	w := &Writer{dir: dir}
	if err := w.probe(); err != nil {
		return nil, fmt.Errorf("probe results directory %q: %w", dir, err)
	}
	return w, nil
}

func (w *Writer) probe() (err error) {
	f, err := w.createTemp()
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
	target, err := w.createTemp()
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

// Destination maps a source basename to its complete result path without collisions.
func (w *Writer) Destination(name string) (string, error) {
	if name == "" || name == "." || name == ".." || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\\x00") {
		return "", fmt.Errorf("invalid source basename %q", name)
	}
	name += ".result.json"
	path := filepath.Join(w.dir, name)
	if err := validateDestination(name, path); err != nil {
		return "", err
	}
	return path, nil
}

// Write syncs a private temporary file before replacing the old result.
// Context cancellation is checked between filesystem operations, not inside syscalls.
func (w *Writer) Write(ctx context.Context, result dsc.Result) (err error) {
	target, err := w.Destination(result.Configuration)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	f, err := w.createTemp()
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
	if err = json.NewEncoder(f).Encode(result); err != nil {
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
	if err = syncDirectory(w.dir); err != nil {
		return fmt.Errorf("replacement is visible but directory durability is uncertain: %w", err)
	}
	return nil
}

func (w *Writer) createTemp() (*os.File, error) {
	f, err := os.CreateTemp(w.dir, ".dscd-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary result in %q: %w", w.dir, err)
	}
	if err = protectFile(f.Name()); err != nil {
		return nil, fmt.Errorf("protect temporary result: %w", errors.Join(err, f.Close(), os.Remove(f.Name())))
	}
	return f, nil
}
