//go:build linux

package reconcile

import "github.com/fsnotify/fsnotify"

func newEventSource() (eventSource, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return eventSource{}, err
	}
	return eventSource{events: w.Events, errors: w.Errors, add: w.Add, remove: w.Remove, close: w.Close}, nil
}
