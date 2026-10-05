//go:build !linux

package details

func ioIdle(int) {}

func idleIO(fn func() error) error { return fn() }
