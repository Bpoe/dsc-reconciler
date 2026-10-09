package config

import (
	"errors"
	"os"
	"path/filepath"
)

func defaultDSCPath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	path := filepath.Join(filepath.Dir(executable), "dsc", "dsc.exe")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return "dsc", nil
	} else if err != nil {
		return "", err
	}
	return path, nil
}
