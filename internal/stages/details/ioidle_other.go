//go:build !linux

package details

func background(fn func()) { fn() }
