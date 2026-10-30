//go:build windows

package cron

// shortcut: no inter-process lock on Windows, where one desktop gateway owns
// the store; add LockFileEx if two processes ever share it there.
func lockStore(string) (func(), error) { return func() {}, nil }
