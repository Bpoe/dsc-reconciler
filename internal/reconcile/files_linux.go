//go:build linux

package reconcile

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func openInputFile(path string) (*os.File, error) { return os.Open(path) }

func unavailableConfiguration(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ELOOP)
}

func openProtectedFile(ctx context.Context, path string) (*os.File, error) {
	// Do not follow a substituted symlink or block opening a substituted FIFO.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if unavailableConfiguration(err) {
		return nil, errStaleInput
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	if err := lockConfiguration(ctx, f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func lockConfiguration(ctx context.Context, f *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return &os.PathError{Op: "flock", Path: f.Name(), Err: err}
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
