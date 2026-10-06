package config

import (
	"os"
	"path/filepath"
)

func defaultDirectories() (string, string) {
	root := os.Getenv("ProgramData")
	if root == "" {
		root = `C:\ProgramData`
	}
	return filepath.Join(root, "dsc", "config.d"), filepath.Join(root, "dsc", "results.d")
}
