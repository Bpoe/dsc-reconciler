package results

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func makeDirectory(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%q is not a directory", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return err
	}
	if err := makeDirectory(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	return errors.Join(syncDirectory(path), syncDirectory(parent))
}

func protectFile(string) error { return nil } // CreateTemp uses 0600.

func validateDestination(name, path string) error {
	if len(name) > 255 {
		return fmt.Errorf("result basename exceeds 255 bytes: %q", name)
	}
	if len(path) >= 4096 {
		return fmt.Errorf("result path exceeds 4095 bytes: %q", path)
	}
	return nil
}

func replaceFile(source, target string) (bool, error) {
	err := os.Rename(source, target)
	return err == nil, err
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
