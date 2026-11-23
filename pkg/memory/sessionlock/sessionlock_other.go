//go:build !unix

package sessionlock

import "time"

// shortcut: no inter-process lock off unix, where one desktop gateway owns sessions/; add LockFileEx if two processes ever share it there.

// Lock holds nothing off unix; names are still checked, so a bad name fails
// on every platform.
type Lock struct{}

func Acquire(_, name string, _ time.Duration) (*Lock, error) { return TryAcquire("", name) }

func TryAcquire(_, name string) (*Lock, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	return &Lock{}, nil
}

func AcquireAll(_ string, names []string, _ bool, _ time.Duration) ([]*Lock, error) {
	for _, name := range names {
		if err := validName(name); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (*Lock) Check() error { return nil }

func (*Lock) MarkRemove() {}

func (*Lock) Lost() bool { return false }

func (*Lock) Release() error { return nil }
