//go:build !linux

package watcher

import "errors"

// The service runs on the NAS (Linux); other systems build and test with a fake source.
func openInotify() (source, error) { return nil, errors.New("watcher: inotify needs Linux") }
