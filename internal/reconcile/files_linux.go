//go:build linux

package reconcile

import "os"

func openInputFile(path string) (*os.File, error) { return os.Open(path) }
