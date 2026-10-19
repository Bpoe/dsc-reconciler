package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os"
)

var errStaleInput = errors.New("configuration changed before protection was acquired")

// openConfiguration protects the current regular file, not an obsolete inode.
// The caller owns the returned file until result publication has finished.
func openConfiguration(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if unavailableConfiguration(err) {
		return nil, errStaleInput
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errStaleInput
	}
	f, err := openProtectedFile(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := currentConfiguration(f, path); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func currentConfiguration(f *os.File, path string) error {
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if unavailableConfiguration(err) {
		return errStaleInput
	}
	if err != nil {
		return fmt.Errorf("inspect protected configuration: %w", err)
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return errStaleInput
	}
	return nil
}
