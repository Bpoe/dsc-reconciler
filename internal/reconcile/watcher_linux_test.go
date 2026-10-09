package reconcile

import "os"

func renameInput(from, to string) error {
	return os.Rename(from, to)
}
